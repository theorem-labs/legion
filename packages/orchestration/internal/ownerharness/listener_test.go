package ownerharness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func publish(t *testing.T, base, topic, key string) Envelope {
	t.Helper()
	payload := `{"topic":"` + topic + `"}`
	body, _ := json.Marshal(map[string]any{"topic": topic, "message": "a notice", "payload": payload, "idempotency_key": key})
	response, err := http.Post(base+"/v1/messages/publish", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope Envelope
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&envelope) != nil || envelope.EventID == "" {
		t.Fatalf("publish on %s: status %d, envelope %+v", topic, response.StatusCode, envelope)
	}
	return envelope
}

type subscription struct {
	lines *bufio.Scanner
	close func()
}

func subscribe(t *testing.T, base string, topics ...string) subscription {
	t.Helper()
	query := url.Values{"topic": topics}
	response, err := http.Get(base + "/harness/v1/subscribe?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(response.Body)
	if !lines.Scan() || !bytes.Contains(lines.Bytes(), []byte(`"subscribed"`)) {
		t.Fatalf("subscription acknowledgement = %q", lines.Text())
	}
	return subscription{lines: lines, close: func() { response.Body.Close() }}
}

func (s subscription) next(t *testing.T) Envelope {
	t.Helper()
	got := make(chan Envelope, 1)
	go func() {
		var envelope Envelope
		if s.lines.Scan() {
			_ = json.Unmarshal(s.lines.Bytes(), &envelope)
		}
		got <- envelope
	}()
	select {
	case envelope := <-got:
		return envelope
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery within 5 s")
	}
	return Envelope{}
}

// A notice reaches only the streams subscribed to its topic when it is published, and is never retained (C21).
func TestListenerDeliversOnlyToSessionsSubscribedWhenItPublishes(t *testing.T) {
	server := httptest.NewServer(NewListener())
	defer server.Close()
	publish(t, server.URL, "notifications.test.a", "before")
	first := subscribe(t, server.URL, "notifications.test.a")
	defer first.close()
	publish(t, server.URL, "notifications.test.b", "other")
	sent := publish(t, server.URL, "notifications.test.a", "key-1")
	if got := first.next(t); got.EventID != sent.EventID || got.Topic != "notifications.test.a" || got.DedupeKey != "publish.key-1" {
		t.Fatalf("delivery = %+v, want %+v", got, sent)
	}
	again := publish(t, server.URL, "notifications.test.a", "key-1")
	if got := first.next(t); got.EventID != again.EventID || got.EventID == sent.EventID || got.DedupeKey != "publish.key-1" {
		t.Fatalf("a repeat under one key = %+v, want a new event with the same dedupe key", got)
	}
}

func TestListenerRefusesATopicOutsideNotifications(t *testing.T) {
	server := httptest.NewServer(NewListener())
	defer server.Close()
	body, _ := json.Marshal(map[string]any{"topic": "example.all", "message": "a notice"})
	response, err := http.Post(server.URL+"/v1/messages/publish", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}
