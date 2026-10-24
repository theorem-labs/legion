package owner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errInjected = errors.New("injected failure")

// topicPublisher refuses every notice on one topic and takes every other.
type topicPublisher struct {
	refuse    string
	published []string
}

func (p *topicPublisher) Publish(_ context.Context, topic, _ string, _ any, _ string) error {
	p.published = append(p.published, topic)
	if topic == p.refuse {
		return errInjected
	}
	return nil
}

// noRetryDelays makes a refused notice's retries immediate for the test.
func noRetryDelays(t *testing.T) {
	saved := noticeRetryDelays
	noticeRetryDelays = [len(noticeRetryDelays)]time.Duration{}
	t.Cleanup(func() { noticeRetryDelays = saved })
}

func deliveryNotices() []Notice {
	return []Notice{
		{Topic: "notifications.example.node.struct:state.needs", Message: "need", Payload: Payload{"item": "a"}, Key: "key-a"},
		{Topic: "notifications.example.node.struct:state.users", Message: "offer", Payload: Payload{"item": "b"}, Key: "key-b"},
	}
}

// Deliver sends each notice, retries a refused one, and journals and returns each one the listener still refuses.
func TestDeliverJournalsAndReturnsTheNoticesTheListenerStillRefuses(t *testing.T) {
	noRetryDelays(t)
	notices := deliveryNotices()
	publisher := &topicPublisher{refuse: notices[0].Topic}
	journal := filepath.Join(t.TempDir(), "state", "undelivered.jsonl")

	undelivered, err := Deliver(context.Background(), publisher, journal, notices)
	if err != nil {
		t.Fatal(err)
	}
	if len(undelivered) != 1 || undelivered[0].Key != notices[0].Key {
		t.Fatalf("undelivered = %+v, want the refused notice", undelivered)
	}
	if got := strings.Count(strings.Join(publisher.published, ","), notices[0].Topic); got != len(noticeRetryDelays)+1 {
		t.Fatalf("the refused notice was tried %d times: %v", got, publisher.published)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entry struct{ Topic, Key, Error string }
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &entry) != nil || entry.Topic != notices[0].Topic || entry.Key != notices[0].Key || entry.Error == "" {
		t.Fatalf("journal = %q, want the refused notice with its error", data)
	}
}

// A journal Deliver cannot write is its error, returned with the notices that were not delivered, the last of them
// the one the journal lost; it writes nothing into the working directory.
func TestDeliverReturnsTheJournalErrorWithTheNoticeItLost(t *testing.T) {
	noRetryDelays(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	notices := deliveryNotices()
	for _, journal := range []string{"", "undelivered.jsonl"} {
		undelivered, err := Deliver(context.Background(), &topicPublisher{refuse: notices[0].Topic}, journal, notices)
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("Deliver with journal %q error = %v, want a refused relative path", journal, err)
		}
		if len(undelivered) != 1 || undelivered[0].Key != notices[0].Key {
			t.Fatalf("undelivered = %+v, want the notice the journal lost", undelivered)
		}
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Fatalf("Deliver wrote into the working directory: %v", entries)
	}
}
