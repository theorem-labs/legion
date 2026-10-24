package owner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/legion/orchestration/ledger"
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

// A failure to reach the ledger is one the caller retries; an answer from the server, or a refusal, is not.
func TestLedgerUnreachableIsAConnectionFailureOrADeadline(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	closed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, refused := ledger.Open(ctx, "postgres://ledger:secret@"+address+"/ledger?sslmode=disable&connect_timeout=5")
	if refused == nil || !LedgerUnreachable(refused) {
		t.Fatalf("a refused connection: %v, unreachable %t", refused, LedgerUnreachable(refused))
	}
	// A server that is shutting down, starting up or out of connections is one to try again too, and so is any
	// connection exception (SQLSTATE class 08).
	for _, err := range []error{
		fmt.Errorf("read: %w", context.DeadlineExceeded),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P02", Message: "terminating connection because of crash of another server process"}),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"}),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "08006", Message: "connection failure"}),
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "08001", Message: "unable to establish connection"}),
	} {
		if !LedgerUnreachable(err) {
			t.Errorf("LedgerUnreachable(%v) = false", err)
		}
	}
	_, unreadable := OpenLedgerFile(ctx, "--ledger-dsn-file", filepath.Join(t.TempDir(), "no-such-dsn"))
	for _, err := range []error{
		// A file's errno satisfies net.Error (syscall.Errno has Timeout and Temporary), yet no ledger was tried.
		unreadable,
		fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}),
		&ledger.NotFoundError{What: "repository", ID: "r"},
		&ledger.Refusal{Reason: ledger.ReasonExists},
		errors.New("parse DSN"),
	} {
		if LedgerUnreachable(err) {
			t.Errorf("LedgerUnreachable(%v) = true", err)
		}
	}
}
