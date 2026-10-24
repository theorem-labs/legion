// Package ownerharness is test support for the owner message test, and only tests import it (guard_test.go checks
// this). It holds stand-ins: a supervisor that starts standing owners in tmux the way the agreed supervisor contracts
// C8, C9 and C33 describe, an HTTP stand-in for the Envoy listener's publish route and its delivery to subscribed
// sessions (C21), and a scripted owner session that follows owner.md. The test binary runs them when linked as
// legion or omp. None of them is the Legion daemon, Envoy or omp: a run against them checks the message-test driver
// and the owner command, and is neither production nor acceptance evidence that the daemon meets C8, C9 or C33.
package ownerharness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Envelope is the part of an Envoy envelope a publish returns and a subscribed session receives.
type Envelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload,omitempty"`
}

// Listener serves POST /v1/messages/publish as the Envoy listener does for an ad-hoc topic (C21) — the
// idempotency key becomes the dedupe key publish.<key>, and the notice reaches only the sessions subscribed to its
// topic at that moment and is never retained —, GET /harness/v1/subscribe?topic=…&session=…&dir=… as a session's own
// subscription: one JSON line that acknowledges it, then one line per delivery, and GET /v1/sessions[?dir=…] as the
// listener lists the sessions it knows, each with its working directory and topics.
type Listener struct {
	mu      sync.Mutex
	streams map[*stream]struct{}
}

type stream struct {
	session   string
	dir       string
	topics    []string
	envelopes chan Envelope
}

// listedSession is one session of GET /v1/sessions, in the listener's field names.
type listedSession struct {
	SessionID string   `json:"session_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
}

// NewListener makes a listener with no subscriptions.
func NewListener() *Listener { return &Listener{streams: map[*stream]struct{}{}} }

func (l *Listener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
		l.publish(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/harness/v1/subscribe":
		l.subscribe(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		l.sessions(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// sessions lists the subscribed sessions whose working directory holds ?dir=, as the Envoy listener filters them.
func (l *Listener) sessions(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("dir")
	listed := []listedSession{}
	l.mu.Lock()
	for s := range l.streams {
		if strings.Contains(s.dir, filter) {
			listed = append(listed, listedSession{SessionID: s.session, Dir: s.dir, Topics: slices.Clone(s.topics)})
		}
	}
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listed)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func newID(prefix string) string {
	var bytes [8]byte
	_, _ = rand.Read(bytes[:])
	return prefix + hex.EncodeToString(bytes[:])
}

func (l *Listener) publish(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Topic          string  `json:"topic"`
		Message        string  `json:"message"`
		Payload        *string `json:"payload"`
		IdempotencyKey string  `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !strings.HasPrefix(request.Topic, "notifications.") || strings.TrimSpace(request.Message) == "" {
		writeError(w, http.StatusBadRequest, "topic must begin with notifications. and message is required")
		return
	}
	envelope := Envelope{
		EventID: newID("evt-"), Source: "agent", Topic: request.Topic, DedupeKey: newID("publish."),
		IssuedAt: time.Now().UnixMilli(), PayloadSummary: strings.SplitN(request.Message, "\n", 2)[0],
	}
	if request.IdempotencyKey != "" {
		envelope.DedupeKey = "publish." + request.IdempotencyKey
	}
	if request.Payload != nil {
		envelope.Payload = *request.Payload
	}
	l.mu.Lock()
	for s := range l.streams {
		if slices.Contains(s.topics, envelope.Topic) {
			select {
			case s.envelopes <- envelope:
			default: // a session that does not read loses the notice, as a slow core NATS subscriber does
			}
		}
	}
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope)
}

func (l *Listener) subscribe(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	topics := query["topic"]
	flusher, ok := w.(http.Flusher)
	if len(topics) == 0 || !ok {
		writeError(w, http.StatusBadRequest, "subscribe takes at least one topic")
		return
	}
	s := &stream{session: query.Get("session"), dir: query.Get("dir"), topics: slices.Clone(topics), envelopes: make(chan Envelope, 1024)}
	l.mu.Lock()
	l.streams[s] = struct{}{}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.streams, s)
		l.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(map[string][]string{"subscribed": topics}); err != nil {
		return
	}
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case envelope := <-s.envelopes:
			if err := encoder.Encode(envelope); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
