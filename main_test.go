package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurabox/internal/tlsutil"
)

// TestHTTPSRedirectAndSecurityEndpoints verifica la redirección de IP sin puerto hacia HTTPS y descarga de CA local.
func TestHTTPSRedirectAndSecurityEndpoints(t *testing.T) {
	cm, err := tlsutil.InitCertManager(t.TempDir(), nil, "", "")
	if err != nil {
		t.Fatalf("InitCertManager falló: %v", err)
	}

	srv := &Server{
		certManager:     cm,
		httpsPort:       "4443",
		httpsPublicPort: "443",
		forceHTTPS:      false,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/security/status", srv.handleSecurityStatus)
	mux.HandleFunc("/api/security/ca.crt", srv.handleDownloadCACert)

	handler := srv.httpsRedirectMiddleware(mux)

	// 1. Petición por puerto 80 (sin puerto en Host -> ej. teclear solo la IP) debe redirigir a https://192.168.1.113/
	reqBare := httptest.NewRequest(http.MethodGet, "http://192.168.1.113/?tab=settings", nil)
	reqBare.Host = "192.168.1.113"
	recBare := httptest.NewRecorder()
	handler.ServeHTTP(recBare, reqBare)
	if recBare.Code != http.StatusTemporaryRedirect {
		t.Fatalf("código esperado 307 para Host sin puerto, obtenido %d", recBare.Code)
	}
	if loc := recBare.Header().Get("Location"); loc != "https://192.168.1.113/?tab=settings" {
		t.Fatalf("Location esperada https://192.168.1.113/?tab=settings, obtenida %q", loc)
	}

	// Verificar que EnsureHost incorporó dinámicamente 192.168.1.113 en el certificado SAN
	hasIP := false
	for _, h := range cm.SANHosts() {
		if h == "192.168.1.113" {
			hasIP = true
			break
		}
	}
	if !hasIP {
		t.Fatalf("se esperaba 192.168.1.113 en SANHosts, obtenido %v", cm.SANHosts())
	}

	// 2. Con FORCE_HTTPS=false y puerto explícito :4080, HTTP debe seguir sirviendo normalmente
	reqPort := httptest.NewRequest(http.MethodGet, "http://192.168.1.113:4080/", nil)
	reqPort.Host = "192.168.1.113:4080"
	recPort := httptest.NewRecorder()
	handler.ServeHTTP(recPort, reqPort)
	if recPort.Code != http.StatusOK {
		t.Fatalf("con FORCE_HTTPS=false y :4080 se esperaba 200, obtenido %d", recPort.Code)
	}

	// 3. Con FORCE_HTTPS=true y httpsPublicPort=4443, :4080 redirige a :4443
	srv.forceHTTPS = true
	srv.httpsPublicPort = "4443"
	recForced := httptest.NewRecorder()
	handler.ServeHTTP(recForced, reqPort)
	if recForced.Code != http.StatusTemporaryRedirect {
		t.Fatalf("con FORCE_HTTPS=true se esperaba 307, obtenido %d", recForced.Code)
	}
	if loc := recForced.Header().Get("Location"); loc != "https://192.168.1.113:4443/" {
		t.Fatalf("Location esperada https://192.168.1.113:4443/, obtenida %q", loc)
	}

	// 4. Descarga de CA y stream HTTP no deben redirigir incluso con FORCE_HTTPS=true
	reqCA := httptest.NewRequest(http.MethodGet, "http://192.168.1.113:4080/api/security/ca.crt", nil)
	reqCA.Host = "192.168.1.113:4080"
	recCA := httptest.NewRecorder()
	handler.ServeHTTP(recCA, reqCA)
	if recCA.Code != http.StatusOK || !strings.Contains(recCA.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("descarga de CA falló (code=%d)", recCA.Code)
	}
}

