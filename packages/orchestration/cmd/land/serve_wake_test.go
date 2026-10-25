package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// lockedBuffer is a buffer that serve writes while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// rounds is each round's output: one JSON list of batches per line.
func (b *lockedBuffer) rounds(t *testing.T) [][]servedBatch {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out [][]servedBatch
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var batches []servedBatch
		if err := json.Unmarshal([]byte(line), &batches); err != nil {
			t.Fatalf("decode serve output %q: %v", line, err)
		}
		out = append(out, batches)
	}
	return out
}

type servedBatch struct {
	ID            string `json:"id"`
	FrozenMembers []struct {
		Migration string `json:"migration"`
	} `json:"frozenMembers"`
}

// formed reports whether a round has formed a batch, and the migrations of the first one it formed.
func formed(rounds [][]servedBatch) (bool, []string) {
	for _, round := range rounds {
		if len(round) != 0 {
			var members []string
			for _, member := range round[0].FrozenMembers {
				members = append(members, member.Migration)
			}
			return true, members
		}
	}
	return false, nil
}

// readyOne opens migration id at head and readies it, as its owner does.
func readyOne(t *testing.T, store *ledger.Store, id, base, head string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.OpenMigration(ctx, ledger.OpenRequest{
		Repo: "owner/repo", ID: id, Owner: "alice", BaseCommit: base, Branch: id, Writes: []string{id + ".lean"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ready(ctx, ledger.ReadyRequest{Repo: "owner/repo", ID: id, Generation: 1, Head: head, Key: "ready-" + id}); err != nil {
		t.Fatal(err)
	}
}

// serveInBackground runs land serve until the test ends, and returns its output.
func serveInBackground(t *testing.T, config string) *lockedBuffer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	output := &lockedBuffer{}
	go func() {
		defer close(done)
		_ = run(ctx, []string{"serve", "--config", config}, output)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return output
}

// waitFor polls until cond holds or within passes.
func waitFor(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// An owner's ready submission wakes a serving program at once: with an hour's interval and no ready migration at
// start, the first round forms nothing, and the ready submission starts the next round, which forms a batch.
func TestServeWakesOnAReadySubmission(t *testing.T) {
	repo, base, heads := readyRepository(t, "first")
	store, dsnFile := servingLedger(t, "owner/repo")
	output := serveInBackground(t, servingConfig(t, "owner/repo", repo, dsnFile, time.Hour))

	if !waitFor(5*time.Minute, func() bool { return len(output.rounds(t)) >= 1 }) {
		t.Fatal("serve never finished its first round")
	}
	if ok, _ := formed(output.rounds(t)); ok {
		t.Fatalf("the first round formed a batch with no ready migration: %+v", output.rounds(t))
	}

	readyOne(t, store, "first", base, heads["first"])
	var members []string
	if !waitFor(5*time.Minute, func() bool { ok, got := formed(output.rounds(t)); members = got; return ok }) {
		t.Fatalf("a ready submission did not wake serve well within its hour's interval: %+v", output.rounds(t))
	}
	if len(members) != 1 || members[0] != "first" {
		t.Fatalf("the woken round formed %v, want first", members)
	}
}

// A ready submission whose notification is missed is still picked up, by the interval: serve runs here with a wake
// source that never wakes, as when the listening connection drops the notification, and the ready migration forms a
// batch within the two-second interval.
func TestServeIntervalPicksUpAMissedNotification(t *testing.T) {
	repo, base, heads := readyRepository(t, "first")
	store, dsnFile := servingLedger(t, "owner/repo")
	cfg, err := readConfig(servingConfig(t, "owner/repo", repo, dsnFile, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	built, err := cfg.program(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	output := &lockedBuffer{}
	silent := make(chan struct{})
	go func() {
		defer close(done)
		_ = serve(ctx, built.runner, serving{repository: "owner/repo", baseBranch: "trunk", maxMembers: 2, interval: 2 * time.Second, wakes: silent}, output)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if !waitFor(5*time.Minute, func() bool { return len(output.rounds(t)) >= 1 }) {
		t.Fatal("serve never finished its first round")
	}
	readyOne(t, store, "first", base, heads["first"])
	if !waitFor(5*time.Minute, func() bool { ok, _ := formed(output.rounds(t)); return ok }) {
		t.Fatalf("the interval did not pick up a ready migration whose notification was missed: %+v", output.rounds(t))
	}
}

// A ready head that cannot be fetched from the fetch URL is returned to its owner with head_unavailable, and the
// batch forms from the other ready migrations.
func TestServeReturnsAHeadThatCannotBeFetched(t *testing.T) {
	ctx := context.Background()
	repo, base, heads := readyRepository(t, "first", "second")
	store, dsnFile := servingLedger(t, "owner/repo")
	readyOne(t, store, "first", base, heads["first"])
	missing := strings.Repeat("1", 40)
	readyOne(t, store, "second", base, missing)

	// The round's build and hasher run in containers, so the window covers them.
	round, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	var output bytes.Buffer
	if err := run(round, []string{"serve", "--config", servingConfig(t, "owner/repo", repo, dsnFile, time.Hour)}, &output); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("serve() error = %v, want the ended context", err)
	}
	m, err := store.Migration(ctx, "owner/repo", "second")
	if err != nil || m.LandingRounds != 1 {
		t.Fatalf("second = %+v (%v), want returned with one round", m, err)
	}
	events, err := store.Events(ctx, "owner/repo", 0, "second")
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, event := range events {
		if event.Kind == "migration.round_failed" {
			var data struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(event.Data, &data)
			reason = data.Reason
		}
	}
	if reason != "head_unavailable" {
		t.Fatalf("second's round reason = %q, want head_unavailable", reason)
	}
	var batches []servedBatch
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(output.String()), "\n", 2)[0]), &batches); err != nil {
		t.Fatalf("decode %q: %v", output.String(), err)
	}
	if len(batches) != 1 || len(batches[0].FrozenMembers) != 1 || batches[0].FrozenMembers[0].Migration != "first" {
		t.Fatalf("serve formed %+v, want one batch of first", batches)
	}
}
