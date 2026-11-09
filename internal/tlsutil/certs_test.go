package tlsutil

import (
	"bytes"
	"crypto/x509"
	"testing"
	"time"
)

// TestCertManagerCAAndDynamicSAN verifica la generación de CA local, validez en iOS/Apple y renovación dinámica de SAN.
func TestCertManagerCAAndDynamicSAN(t *testing.T) {
	certsDir := t.TempDir()

	cm, err := InitCertManager(certsDir, []string{"10.0.0.50", "jukebox.local"}, "", "")
	if err != nil {
		t.Fatalf("InitCertManager falló: %v", err)
	}
	if !cm.HasCA() || len(cm.CACertPEM()) == 0 {
		t.Fatalf("se esperaba CA local generada y disponible")
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cm.CACertPEM()) {
		t.Fatalf("no se pudo cargar aurabox-ca.crt en CertPool")
	}

	pair, err := cm.GetCertificate(nil)
	if err != nil || pair == nil || pair.Leaf == nil {
		t.Fatalf("GetCertificate falló: %v", err)
	}

	// Validar límite Apple/iOS de <= 825 días para certificado hoja
	validity := pair.Leaf.NotAfter.Sub(pair.Leaf.NotBefore)
	if validity > 825*24*time.Hour {
		t.Fatalf("el certificado hoja excede 825 días (%v)", validity)
	}

	// Verificar cadena contra la CA para extraHosts iniciales
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "aurabox.local"}); err != nil {
		t.Fatalf("falló verificación DNS aurabox.local: %v", err)
	}
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "10.0.0.50"}); err != nil {
		t.Fatalf("falló verificación IP 10.0.0.50: %v", err)
	}

	// Simular acceso desde nueva IP LAN en caliente (EnsureHost) y validar que la misma CA siga confiando en el nuevo leaf
	cm.EnsureHost("192.168.1.113:4080")
	pair2, err := cm.GetCertificate(nil)
	if err != nil || pair2 == nil || pair2.Leaf == nil {
		t.Fatalf("GetCertificate tras EnsureHost falló: %v", err)
	}
	if _, err := pair2.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "192.168.1.113"}); err != nil {
		t.Fatalf("el nuevo certificado no verificó IP 192.168.1.113 bajo la CA original: %v", err)
	}

	// Reabrir CertManager desde disco y comprobar que reutiliza la misma CA sin invalidar dispositivos ya emparejados
	cmReloaded, err := InitCertManager(certsDir, []string{"192.168.1.113"}, "", "")
	if err != nil {
		t.Fatalf("InitCertManager al recargar falló: %v", err)
	}
	if !bytes.Equal(cm.CACertPEM(), cmReloaded.CACertPEM()) {
		t.Fatalf("la CA local cambió al recargar desde disco")
	}
}

