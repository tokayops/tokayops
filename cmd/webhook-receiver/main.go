package main

import (
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

// --- Models ---

type ReceivedWebhook struct {
	ID              string              `json:"id"`
	ReceivedAt      time.Time           `json:"received_at"`
	Method          string              `json:"method"`
	Path            string              `json:"path"`
	Headers         map[string][]string `json:"headers"`
	Body            json.RawMessage     `json:"body"`
	ContentLength   int                 `json:"content_length"`
	EventType       string              `json:"event_type"`
	EventID         string              `json:"event_id"`
	DeliveryTS      string              `json:"delivery_ts"`
	Signature       string              `json:"signature"`
	AlertGroupTitle string              `json:"alert_group_title"`
	AlertGroupID    string              `json:"alert_group_id"`
	Status          string              `json:"status"`
	Severity        string              `json:"severity"`
	TeamName        string              `json:"team_name"`
	ActorName       string              `json:"actor_name"`
	SignatureStatus string              `json:"signature_status"` // valid | invalid | unchecked
}

// Lightweight payload struct for extracting display fields (mirrors model.WebhookEventPayload).
type webhookPayload struct {
	Event      string `json:"event"`
	AlertGroup struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Status   string `json:"status"`
		Severity string `json:"severity"`
		TeamName string `json:"team_name"`
	} `json:"alert_group"`
	Actor struct {
		Name string `json:"name"`
	} `json:"actor"`
}

// --- In-memory Store ---

type Store struct {
	mu       sync.RWMutex
	webhooks []ReceivedWebhook
	counter  int
	maxSize  int
}

func NewStore(maxSize int) *Store {
	return &Store{maxSize: maxSize}
}

func (s *Store) Add(wh ReceivedWebhook) ReceivedWebhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	wh.ID = strconv.Itoa(s.counter)
	s.webhooks = append([]ReceivedWebhook{wh}, s.webhooks...)
	if len(s.webhooks) > s.maxSize {
		s.webhooks = s.webhooks[:s.maxSize]
	}
	return wh
}

func (s *Store) List() []ReceivedWebhook {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ReceivedWebhook, len(s.webhooks))
	copy(result, s.webhooks)
	return result
}

func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhooks = nil
	s.counter = 0
}

// --- SSE Broker ---

type SSEMessage struct {
	Event string          `json:"-"`
	Data  json.RawMessage `json:"data"`
}

type SSEBroker struct {
	mu      sync.Mutex
	clients map[chan SSEMessage]struct{}
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{clients: make(map[chan SSEMessage]struct{})}
}

func (b *SSEBroker) Subscribe() chan SSEMessage {
	ch := make(chan SSEMessage, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *SSEBroker) Unsubscribe(ch chan SSEMessage) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *SSEBroker) Broadcast(msg SSEMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
			// slow client, drop message
		}
	}
}

// --- HMAC Verification ---
// Mirrors internal/outbox/sender.go:signPayload (line 161-167).

func verifySignature(timestamp string, body []byte, secret, providedSig string) bool {
	prefix := "sha256="
	if !strings.HasPrefix(providedSig, prefix) {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	computed := prefix + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(computed), []byte(providedSig))
}

// --- Handlers ---

func webhookHandler(store *Store, broker *SSEBroker, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB limit
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		wh := ReceivedWebhook{
			ReceivedAt:    time.Now(),
			Method:        r.Method,
			Path:          r.URL.Path,
			Headers:       r.Header,
			ContentLength: len(body),
			EventType:     r.Header.Get("X-Tokay-Event"),
			EventID:       r.Header.Get("X-Tokay-Event-ID"),
			DeliveryTS:    r.Header.Get("X-Tokay-Timestamp"),
			Signature:     r.Header.Get("X-Tokay-Signature"),
		}

		// Store body as raw JSON, or as a JSON string if not valid JSON
		if json.Valid(body) {
			wh.Body = body
		} else {
			wh.Body, _ = json.Marshal(string(body))
		}

		// HMAC verification
		if secret == "" {
			wh.SignatureStatus = "unchecked"
		} else if wh.Signature == "" {
			wh.SignatureStatus = "unchecked"
		} else if verifySignature(wh.DeliveryTS, body, secret, wh.Signature) {
			wh.SignatureStatus = "valid"
		} else {
			wh.SignatureStatus = "invalid"
		}

		// Extract display fields from payload
		var p webhookPayload
		if json.Unmarshal(body, &p) == nil {
			wh.AlertGroupTitle = p.AlertGroup.Title
			wh.AlertGroupID = p.AlertGroup.ID
			wh.Status = p.AlertGroup.Status
			wh.Severity = p.AlertGroup.Severity
			wh.TeamName = p.AlertGroup.TeamName
			wh.ActorName = p.Actor.Name
		}

		saved := store.Add(wh)

		// Broadcast to SSE clients
		data, _ := json.Marshal(saved)
		broker.Broadcast(SSEMessage{Event: "webhook", Data: data})

		sigLabel := saved.SignatureStatus
		eventLabel := saved.EventType
		if eventLabel == "" {
			eventLabel = "unknown"
		}
		log.Printf("#%s  %s  %s  [sig: %s]  %s", saved.ID, eventLabel, saved.AlertGroupTitle, sigLabel, r.RemoteAddr)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func listHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.List())
	}
}

func clearHandler(store *Store, broker *SSEBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store.Clear()
		broker.Broadcast(SSEMessage{Event: "clear", Data: json.RawMessage(`{}`)})
		log.Println("Store cleared")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func sseHandler(broker *SSEBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := broker.Subscribe()
		defer broker.Unsubscribe(ch)

		// Send initial keepalive
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
				flusher.Flush()
			}
		}
	}
}

func uiHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}
}

// Root handler: POST → webhook, GET → UI
func rootHandler(webhookH, uiH http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			webhookH(w, r)
			return
		}
		uiH(w, r)
	}
}

func main() {
	port := flag.Int("port", 9999, "Port to listen on")
	secret := flag.String("secret", "", "HMAC secret for signature verification (optional)")
	flag.Parse()

	store := NewStore(1000)
	broker := NewSSEBroker()

	whHandler := webhookHandler(store, broker, *secret)
	ui := uiHandler()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/webhooks", listHandler(store))
	mux.HandleFunc("DELETE /api/webhooks", clearHandler(store, broker))
	mux.HandleFunc("GET /api/events", sseHandler(broker))
	mux.HandleFunc("/", rootHandler(whHandler, ui))

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Webhook Receiver listening on http://localhost%s", addr)
	if *secret != "" {
		log.Printf("HMAC signature verification: enabled")
	} else {
		log.Printf("HMAC signature verification: disabled (use --secret to enable)")
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}
