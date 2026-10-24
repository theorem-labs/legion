package messagetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// publishes is a stand-in for the listener's publish route: it gives each notice an event id and keeps the notices.
type publishes struct {
	mu      sync.Mutex
	notices []map[string]string // event id, topic and payload of each publish
}

func (p *publishes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Topic   string `json:"topic"`
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	id := fmt.Sprintf("evt-%d", len(p.notices)+1)
	p.notices = append(p.notices, map[string]string{"id": id, "topic": request.Topic, "payload": request.Payload})
	p.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"event_id": id})
}

func (p *publishes) published() []map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]string{}, p.notices...)
}

// Acceptance run 7 stalled here: the target answered r3, completed r2 as superseded, and both notices reached its
// session, but the owner model ended its turn having recorded one of them, and the driver waited for the other record
// until its 15-minute timeout. The step ends on the ledger and the session's transcript alone; the owner here records
// neither notice.
func TestOutOfOrderEndsOnTheLedgerAndTheTranscriptWhateverTheOwnerRecords(t *testing.T) {
	ctx := t.Context()
	store, err := ledger.Open(ctx, testpg.Database(t, "message_test_out_of_order"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	listener := &publishes{}
	server := httptest.NewServer(listener)
	defer server.Close()
	routing, err := ownerpkg.NewRouting("notifications.example-message-test.r.", ownerpkg.FactOwners{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	transcript := filepath.Join(dir, "2026-10-24T05-14-44-553Z_s.jsonl")
	writeTranscript(t, transcript, titleLine, headerLine, "")
	d := &driver{
		opts: Options{Progress: io.Discard, Timeouts: Timeouts{Item: 30 * time.Second, Poll: 10 * time.Millisecond}},
		run:  "r", nodes: []string{fn, st}, source: fn, target: st, store: store, routing: routing, report: &Report{},
		publisher: ownerpkg.NewHTTPPublisher(server.URL, ""), bare: filepath.Join(dir, "no-repo.git"),
		subscribe: map[string][]string{st: {routing.NeedsTopic(st)}}, transcriptDirs: map[string][]string{st: {dir}},
	}
	d.expectAnswered("r1") // the duplicates step's need: the out-of-order step's answer queues the second offer

	owner := make(chan error, 1)
	go func() {
		owner <- answerOutOfOrder(ctx, store, listener, transcript, d.need("r3"), d.need("r2"), "offer:"+edge+"@r2")
	}()
	if err := d.outOfOrder(ctx); err != nil {
		t.Fatalf("the out-of-order step: %v", err)
	}
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
}

// answerOutOfOrder is the target owner of the out-of-order step: once both needs and their notices are out, both
// notices reach its session's transcript, and it answers r3 with its follow-up offer and completes r2 as superseded.
// It records no delivery of its own.
func answerOutOfOrder(ctx context.Context, store *ledger.Store, listener *publishes, transcript, r3, r2, offer string) error {
	for len(listener.published()) < 2 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, notice := range listener.published() {
		content := "envoy:\n  from: agent\n  id: " + notice["id"] + "\n  message:\n    run: r\n    step: out of order\n    topic: " + notice["topic"] + "\n    type: message-test"
		line, _ := json.Marshal(map[string]any{"type": "custom_message", "customType": "envoy-message", "content": content, "timestamp": time.Now().UTC()})
		if _, err := file.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	lease, err := store.Claim(ctx, "r", r3, st, 0)
	if err != nil {
		return err
	}
	if err := store.Enqueue(ctx, "r", offer, fn); err != nil {
		return err
	}
	if err := store.Complete(ctx, "r", r3, st, lease.Generation, "answered"); err != nil {
		return err
	}
	if lease, err = store.Claim(ctx, "r", r2, st, 0); err != nil {
		return err
	}
	return store.Complete(ctx, "r", r2, st, lease.Generation, "superseded:r3")
}
