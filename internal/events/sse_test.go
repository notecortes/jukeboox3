package events

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEServeHTTPHeadersAndBroadcast verifica cabeceras SSE HTTP/1.1 vs HTTP/2, entrega inicial y retransmisión en vivo.
func TestSSEServeHTTPHeadersAndBroadcast(t *testing.T) {
	hub := NewHub()
	hub.SetOnConnect(func(send func(string, interface{})) {
		send("player_state", map[string]string{"status": "play"})
	})

	// 1. Verificar ausencia de cabecera "Connection: keep-alive" sobre HTTP/2
	ctx2, cancel2 := context.WithCancel(context.Background())
	req2 := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx2)
	req2.ProtoMajor = 2
	req2.ProtoMinor = 0
	rec2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		hub.ServeHTTP(rec2, req2)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel2()
	<-done2

	if rec2.Header().Get("Connection") != "" {
		t.Fatalf("HTTP/2 no debe emitir cabecera Connection, recibida: %q", rec2.Header().Get("Connection"))
	}
	if rec2.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering esperado 'no', obtenido %q", rec2.Header().Get("X-Accel-Buffering"))
	}

	// 2. Verificar flujo real de eventos connect -> player_state -> queue_update sobre HTTP server
	ts := httptest.NewServer(hub)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequest falló: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/events falló: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type SSE inesperado: %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	var collected []string
	readUntil := func(substr string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("error leyendo SSE buscando %q: %v (recibido: %s)", substr, readErr, strings.Join(collected, ""))
			}
			collected = append(collected, line)
			if strings.Contains(strings.Join(collected, ""), substr) {
				return
			}
		}
		t.Fatalf("timeout esperando evento %q en SSE; recibido:\n%s", substr, strings.Join(collected, ""))
	}

	readUntil("event: connected")
	readUntil("event: player_state")

	hub.Broadcast("queue_update", map[string]int{"length": 4})
	readUntil("event: queue_update")
}

