package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxDynamicSANs = 32

var reLocalMDNSHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.local$`)

// CertManager gestiona la autoridad certificadora local y los certificados TLS dinámicos de AuraBox.
type CertManager struct {
	mu          sync.RWMutex
	certsDir    string
	caCert      *x509.Certificate
	caKey       *ecdsa.PrivateKey
	caPEM       []byte
	tlsCert     *tls.Certificate
	dnsNames    map[string]bool
	ipAddresses map[string]bool
	dynamicSANs int
	customCert  bool
}

// InitCertManager inicializa (o carga) la CA local y emite el certificado del servidor con SAN dinámicos.
func InitCertManager(certsDir string, extraHosts []string, customCertFile string, customKeyFile string) (*CertManager, error) {
	if err := os.MkdirAll(certsDir, 0755); err != nil {
		return nil, fmt.Errorf("no se pudo crear directorio de certificados: %w", err)
	}

	cm := &CertManager{
		certsDir:    certsDir,
		dnsNames:    make(map[string]bool),
		ipAddresses: make(map[string]bool),
	}

	// Si el usuario aporta un certificado propio (ej. Let's Encrypt o Tailscale), usarlo directamente
	if customCertFile != "" && customKeyFile != "" {
		pair, err := tls.LoadX509KeyPair(customCertFile, customKeyFile)
		if err != nil {
			return nil, fmt.Errorf("error cargando certificado personalizado: %w", err)
		}
		cm.tlsCert = &pair
		cm.customCert = true
		return cm, nil
	}

	// Cargar o crear la CA local persistente para poder instalarla una vez en móviles/PC
	if err := cm.loadOrCreateCA(); err != nil {
		return nil, err
	}

	// Registrar nombres e IPs base conocidos
	cm.addHostLocked("localhost")
	cm.addHostLocked("aurabox")
	cm.addHostLocked("aurabox.local")
	cm.addHostLocked("127.0.0.1")
	cm.addHostLocked("::1")

	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		cm.addHostLocked(hostname)
		if !strings.Contains(hostname, ".") {
			cm.addHostLocked(hostname + ".local")
		}
	}

	for _, ip := range detectInterfaceIPs() {
		cm.addHostLocked(ip)
	}
	for _, h := range extraHosts {
		cm.addHostLocked(h)
	}

	if err := cm.loadOrIssueLeafLocked(); err != nil {
		return nil, err
	}

	return cm, nil
}

// GetCertificate devuelve el certificado TLS activo para cada handshake entrante.
func (cm *CertManager) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if cm.tlsCert == nil {
		return nil, fmt.Errorf("certificado TLS no disponible")
	}
	return cm.tlsCert, nil
}

// TLSConfig construye la configuración TLS audiófila segura (TLS 1.2+) respaldada por CertManager.
func (cm *CertManager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cm.GetCertificate,
	}
}

// isAllowedDynamicHost valida que un host candidato a SAN dinámico sea una IP privada/loopback o un nombre mDNS .local válido.
func isAllowedDynamicHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	return reLocalMDNSHost.MatchString(lower)
}

// EnsureHost verifica que el host o IP local solicitado figure en el certificado activo; si falta y cabe en el límite, lo re-emite.
func (cm *CertManager) EnsureHost(rawHost string) {
	host := extractHostOnly(rawHost)
	if host == "" {
		return
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.customCert {
		return
	}

	ip := net.ParseIP(host)
	if ip != nil {
		norm := ip.String()
		if cm.ipAddresses[norm] {
			return
		}
		if !isAllowedDynamicHost(norm) || cm.dynamicSANs >= maxDynamicSANs {
			return
		}
		cm.ipAddresses[norm] = true
		cm.dynamicSANs++
	} else {
		lower := strings.ToLower(host)
		if cm.dnsNames[lower] {
			return
		}
		if !isAllowedDynamicHost(lower) || cm.dynamicSANs >= maxDynamicSANs {
			return
		}
		cm.dnsNames[lower] = true
		cm.dynamicSANs++
	}

	_ = cm.issueLeafLocked()
}

// CACertPEM devuelve los bytes PEM de la CA raíz local para su descarga e instalación opcional en dispositivos.
func (cm *CertManager) CACertPEM() []byte {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if len(cm.caPEM) == 0 {
		return nil
	}
	out := make([]byte, len(cm.caPEM))
	copy(out, cm.caPEM)
	return out
}

// HasCA indica si existe una CA local lista para descargar desde la API.
func (cm *CertManager) HasCA() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return len(cm.caPEM) > 0
}

// SANHosts devuelve la lista ordenada de nombres DNS e IPs incluidos actualmente en el certificado.
func (cm *CertManager) SANHosts() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var list []string
	for h := range cm.dnsNames {
		list = append(list, h)
	}
	for ip := range cm.ipAddresses {
		list = append(list, ip)
	}
	sort.Strings(list)
	return list
}

// addHostLocked normaliza y añade un hostname o dirección IP al mapa de SANs bajo bloqueo de escritura.
func (cm *CertManager) addHostLocked(raw string) {
	host := extractHostOnly(raw)
	if host == "" {
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		cm.ipAddresses[ip.String()] = true
		return
	}
	cm.dnsNames[strings.ToLower(host)] = true
}

// loadOrCreateCA carga una CA local existente desde disco o genera una nueva válida por 10 años.
func (cm *CertManager) loadOrCreateCA() error {
	caCertPath := filepath.Join(cm.certsDir, "aurabox-ca.crt")
	caKeyPath := filepath.Join(cm.certsDir, "aurabox-ca.key")

	certPEM, errCert := os.ReadFile(caCertPath)
	keyPEM, errKey := os.ReadFile(caKeyPath)
	if errCert == nil && errKey == nil {
		certBlock, _ := pem.Decode(certPEM)
		keyBlock, _ := pem.Decode(keyPEM)
		if certBlock != nil && keyBlock != nil {
			parsedCert, err1 := x509.ParseCertificate(certBlock.Bytes)
			parsedKey, err2 := x509.ParseECPrivateKey(keyBlock.Bytes)
			if err1 == nil && err2 == nil && parsedCert.IsCA && time.Now().Add(24*time.Hour).Before(parsedCert.NotAfter) {
				cm.caCert = parsedCert
				cm.caKey = parsedKey
				cm.caPEM = certPEM
				return nil
			}
		}
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("error generando clave privada CA: %w", err)
	}

	serial, err := randomSerialNumber()
	if err != nil {
		return fmt.Errorf("error generando serial CA: %w", err)
	}

	subjKeyID := computeSubjectKeyID(&privKey.PublicKey)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "AuraBox Local Root CA",
			Organization: []string{"AuraBox Hi-Fi"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          subjKeyID,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &privKey.PublicKey, privKey)
	if err != nil {
		return fmt.Errorf("error firmando certificado CA: %w", err)
	}

	parsedCA, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return fmt.Errorf("error analizando CA generada: %w", err)
	}

	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("error serializando clave CA: %w", err)
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(caCertPath, certOut, 0644); err != nil {
		return fmt.Errorf("error guardando aurabox-ca.crt: %w", err)
	}
	if err := os.WriteFile(caKeyPath, keyOut, 0600); err != nil {
		return fmt.Errorf("error guardando aurabox-ca.key: %w", err)
	}

	cm.caCert = parsedCA
	cm.caKey = privKey
	cm.caPEM = certOut
	return nil
}

// loadOrIssueLeafLocked reutiliza el certificado de servidor existente si cubre los SANs actuales y sigue vigente.
func (cm *CertManager) loadOrIssueLeafLocked() error {
	leafCertPath := filepath.Join(cm.certsDir, "aurabox.crt")
	leafKeyPath := filepath.Join(cm.certsDir, "aurabox.key")

	if pair, err := tls.LoadX509KeyPair(leafCertPath, leafKeyPath); err == nil && len(pair.Certificate) > 0 {
		if leaf, parseErr := x509.ParseCertificate(pair.Certificate[0]); parseErr == nil {
			// Verificar vigencia (al menos 30 días de margen) y cadena contra nuestra CA actual
			if time.Now().Add(30*24*time.Hour).Before(leaf.NotAfter) && cm.leafMatchesSANs(leaf) {
				roots := x509.NewCertPool()
				roots.AddCert(cm.caCert)
				if _, verifyErr := leaf.Verify(x509.VerifyOptions{Roots: roots}); verifyErr == nil {
					pair.Leaf = leaf
					cm.tlsCert = &pair
					return nil
				}
			}
		}
	}

	return cm.issueLeafLocked()
}

// leafMatchesSANs comprueba que todos los nombres e IPs registrados estén presentes en el certificado existente.
func (cm *CertManager) leafMatchesSANs(leaf *x509.Certificate) bool {
	existingDNS := make(map[string]bool, len(leaf.DNSNames))
	for _, d := range leaf.DNSNames {
		existingDNS[strings.ToLower(d)] = true
	}
	existingIP := make(map[string]bool, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		existingIP[ip.String()] = true
	}

	for d := range cm.dnsNames {
		if !existingDNS[d] {
			return false
		}
	}
	for ip := range cm.ipAddresses {
		if !existingIP[ip] {
			return false
		}
	}
	return true
}

// issueLeafLocked genera y firma con la CA local un nuevo certificado hoja válido durante 800 días.
func (cm *CertManager) issueLeafLocked() error {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("error generando clave de servidor: %w", err)
	}

	serial, err := randomSerialNumber()
	if err != nil {
		return fmt.Errorf("error generando serial de servidor: %w", err)
	}

	dnsList := make([]string, 0, len(cm.dnsNames))
	for d := range cm.dnsNames {
		dnsList = append(dnsList, d)
	}
	sort.Strings(dnsList)

	ipList := make([]net.IP, 0, len(cm.ipAddresses))
	for ipStr := range cm.ipAddresses {
		if parsed := net.ParseIP(ipStr); parsed != nil {
			ipList = append(ipList, parsed)
		}
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "aurabox.local",
			Organization: []string{"AuraBox Hi-Fi"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(0, 0, 800), // <= 825 días para compatibilidad total con iOS/macOS
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsList,
		IPAddresses:           ipList,
		SubjectKeyId:          computeSubjectKeyID(&privKey.PublicKey),
		AuthorityKeyId:        cm.caCert.SubjectKeyId,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, cm.caCert, &privKey.PublicKey, cm.caKey)
	if err != nil {
		return fmt.Errorf("error firmando certificado servidor: %w", err)
	}

	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("error serializando clave servidor: %w", err)
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	leafCertPath := filepath.Join(cm.certsDir, "aurabox.crt")
	leafKeyPath := filepath.Join(cm.certsDir, "aurabox.key")
	_ = os.WriteFile(leafCertPath, certOut, 0644)
	_ = os.WriteFile(leafKeyPath, keyOut, 0600)

	pair, err := tls.X509KeyPair(certOut, keyOut)
	if err != nil {
		return fmt.Errorf("error cargando par TLS generado: %w", err)
	}
	if parsed, err := x509.ParseCertificate(derBytes); err == nil {
		pair.Leaf = parsed
	}
	cm.tlsCert = &pair
	return nil
}

// extractHostOnly limpia el puerto o corchetes IPv6 de una cabecera Host o cadena de entrada.
func extractHostOnly(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return strings.TrimSpace(s)
}

// detectInterfaceIPs enumera las direcciones IP locales activas del contenedor o host para incluirlas como SAN.
func detectInterfaceIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// randomSerialNumber genera un número de serie positivo (>= 1) de 128 bits conforme a RFC 5280.
func randomSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 127)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

// computeSubjectKeyID calcula el identificador SHA-1 estándar de la clave pública ECDSA.
func computeSubjectKeyID(pub *ecdsa.PublicKey) []byte {
	raw := elliptic.Marshal(pub.Curve, pub.X, pub.Y)
	sum := sha1.Sum(raw)
	return sum[:]
}

