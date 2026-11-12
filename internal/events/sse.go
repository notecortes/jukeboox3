package events

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	maxSSEClients   = 64
	sseWriteTimeout = 10 * time.Second
)

// EventMessage encapsula un mensaje tipado para los clientes SSE conectados.
type EventMessage struct {
	Type      string      `json:"type"`
	Timestamp int64       `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Hub gestiona las suscripciones de clientes en tiempo real y el despacho de eventos.
type Hub struct {
	clients   map[chan EventMessage]bool
	onConnect func(send func(eventType string, data interface{}))
	mu        sync.RWMutex
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

// SetOnConnect registra un callback que envía estado inicial al cliente recién suscrito por SSE.
func (h *Hub) SetOnConnect(fn func(send func(eventType string, data interface{}))) {
	h.mu.Lock()
	h.onConnect = fn
	h.mu.Unlock()
}

// Subscribe registra un nuevo canal de cliente para recibir transmisiones SSE.
func (h *Hub) Subscribe() chan EventMessage {
	ch, _ := h.subscribeBounded()
	return ch
}

// subscribeBounded registra un nuevo cliente SSE respetando el tope máximo concurrente para proteger la RAM.
func (h *Hub) subscribeBounded() (chan EventMessage, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxSSEClients {
		return nil, false
	}
	ch := make(chan EventMessage, 64)
	h.clients[ch] = true
	return ch, true
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

// clearConnectionDeadlines desactiva timeouts de lectura/escritura por petición en flujos largos SSE.
func clearConnectionDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}

// writeSSEFrame escribe un evento SSE aplicando timeout de escritura acotado y limpiándolo tras hacer flush.
func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, eventType, payload string) error {
	if rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		defer func() {
			_ = rc.SetWriteDeadline(time.Time{})
		}()
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// ServeHTTP atiende la conexión SSE entrante (`GET /api/events`).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming no soportado por el navegador", http.StatusBadRequest)
		return
	}

	ch, accepted := h.subscribeBounded()
	if !accepted {
		http.Error(w, "Demasiados clientes SSE conectados", http.StatusServiceUnavailable)
		return
	}
	defer h.Unsubscribe(ch)

	clearConnectionDeadlines(w)
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	if r.ProtoMajor < 2 {
		w.Header().Set("Connection", "keep-alive")
	}
	w.Header().Set("X-Accel-Buffering", "no")

	// Confirmar conexión inmediata al cliente
	if err := writeSSEFrame(w, flusher, rc, "connected", `{"status":"ok"}`); err != nil {
		return
	}

	// Enviar estado inicial directo al cliente conectado sin retransmitir al resto
	h.mu.RLock()
	onConnect := h.onConnect
	h.mu.RUnlock()
	if onConnect != nil {
		writeFailed := false
		onConnect(func(eventType string, data interface{}) {
			if writeFailed {
				return
			}
			dataBytes, err := json.Marshal(data)
			if err != nil {
				log.Printf("[SSE] Error al serializar estado inicial %s: %v", eventType, err)
				return
			}
			if wErr := writeSSEFrame(w, flusher, rc, eventType, string(dataBytes)); wErr != nil {
				writeFailed = true
				return
			}
		})
		if writeFailed {
			return
		}
	}

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
			if wErr := writeSSEFrame(w, flusher, rc, msg.Type, string(dataBytes)); wErr != nil {
				return
			}
		}
	}
}

