package events

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// EventMessage encapsula un mensaje tipado para los clientes SSE conectados.
type EventMessage struct {
	Type      string      `json:"type"`
	Timestamp int64       `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Hub gestiona las suscripciones de clientes en tiempo real y el despacho de eventos.
type Hub struct {
	clients map[chan EventMessage]bool
	mu      sync.RWMutex
}

// NewHub inicializa un nuevo distribuidor de eventos SSE.
func NewHub() *Hub {
	h := &Hub{
		clients: make(map[chan EventMessage]bool),
	}
	// Iniciar pinger periódico para mantener vivas las conexiones móviles
	go h.startKeepAlive()
	return h
}

// Subscribe registra un nuevo canal de cliente para recibir transmisiones SSE.
func (h *Hub) Subscribe() chan EventMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan EventMessage, 64)
	h.clients[ch] = true
	return ch
}

// Unsubscribe elimina un canal de cliente desconectado.
func (h *Hub) Unsubscribe(ch chan EventMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
}

// Broadcast envía un evento a todos los clientes conectados de manera no bloqueante.
func (h *Hub) Broadcast(eventType string, data interface{}) {
	msg := EventMessage{
		Type:      eventType,
		Timestamp: time.Now().UnixMilli(),
		Data:      data,
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			// Si el cliente está bloqueado, descartar para no saturar memoria
		}
	}
}

// startKeepAlive emite un ping de mantenimiento cada 15 segundos para evitar timeouts en navegadores móviles.
func (h *Hub) startKeepAlive() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.Broadcast("ping", map[string]int64{"time": time.Now().UnixMilli()})
	}
}

// ServeHTTP atiende la conexión SSE entrante (`GET /api/events`).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming no soportado por el navegador", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := h.Subscribe()
	defer h.Unsubscribe(ch)

	// Confirmar conexión inmediata al cliente
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"ok\"}\n\n")
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			dataBytes, err := json.Marshal(msg.Data)
			if err != nil {
				log.Printf("[SSE] Error al serializar evento %s: %v", msg.Type, err)
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Type, string(dataBytes))
			flusher.Flush()
		}
	}
}

