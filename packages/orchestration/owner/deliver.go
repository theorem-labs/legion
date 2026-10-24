package owner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

var (
	// ErrIDOwner reports a follow-up identifier that has already been assigned to another owner.
	ErrIDOwner = errors.New("work item belongs to another owner")

	noticeRetryDelays = [...]time.Duration{time.Second, 4 * time.Second, 16 * time.Second}
)

// Payload is the structured content of an Envoy notice.
type Payload map[string]any

// Notice is one Envoy delivery that reports a committed record.
type Notice struct {
	Topic, Message string
	Payload        Payload
	Key            string
}

// Deliver sends each notice through envoy, retrying a refused one at 1 s, 4 s and 16 s, and appends each one that
// envoy still refuses to the journal of undelivered notices at journal: an absolute path, a deployment's
// <state_dir>/undelivered.jsonl. It returns the notices envoy refused, in order. Its error is a journal it could not
// write, and the notices it returns then end with the one that journal lost.
func Deliver(ctx context.Context, envoy Publisher, journal string, notices []Notice) ([]Notice, error) {
	var undelivered []Notice
	for _, notice := range notices {
		publishErr := publish(ctx, envoy, notice)
		if publishErr == nil {
			continue
		}
		undelivered = append(undelivered, notice)
		if err := appendJournal(journal, notice, publishErr); err != nil {
			return undelivered, err
		}
	}
	return undelivered, nil
}

func publish(ctx context.Context, envoy Publisher, notice Notice) error {
	if strings.TrimSpace(notice.Key) == "" {
		return fmt.Errorf("publish notice on %q: empty idempotency key", notice.Topic)
	}

	var err error
	for attempt := 0; attempt <= len(noticeRetryDelays); attempt++ {
		err = envoy.Publish(ctx, notice.Topic, notice.Message, notice.Payload, notice.Key)
		if err == nil {
			return nil
		}
		if attempt == len(noticeRetryDelays) {
			break
		}
		if err := wait(ctx, noticeRetryDelays[attempt]); err != nil {
			return fmt.Errorf("retry notice on %q: %w", notice.Topic, err)
		}
	}
	return fmt.Errorf("publish notice on %q after %d attempts: %w", notice.Topic, len(noticeRetryDelays)+1, err)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func appendJournal(path string, notice Notice, publishErr error) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the notice journal %q is not an absolute path: it is the deployment's <state_dir>/undelivered.jsonl", path)
	}
	entry, err := json.Marshal(struct {
		Topic   string  `json:"topic"`
		Message string  `json:"message"`
		Payload Payload `json:"payload"`
		Key     string  `json:"key"`
		Error   string  `json:"error"`
	}{
		Topic: notice.Topic, Message: notice.Message, Payload: notice.Payload, Key: notice.Key, Error: publishErr.Error(),
	})
	if err != nil {
		return fmt.Errorf("encode undelivered notice on %q: %w", notice.Topic, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("make notice journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open notice journal: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(entry, '\n')); err != nil {
		return fmt.Errorf("write notice journal: %w", err)
	}
	return nil
}

// OpenLedgerFile opens the ledger whose DSN the file dsnFile holds. source names where dsnFile came from (a flag, or
// the deployment's ledger_dsn_file), so an error says which setting to fix.
func OpenLedgerFile(ctx context.Context, source, dsnFile string) (*ledger.Store, error) {
	dsn, err := os.ReadFile(dsnFile)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", source, dsnFile, err)
	}
	if strings.TrimSpace(string(dsn)) == "" {
		return nil, fmt.Errorf("%s %q is empty", source, dsnFile)
	}
	store, err := ledger.Open(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return nil, fmt.Errorf("open ledger from %s: %w", source, err)
	}
	return store, nil
}
