package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

type roundCLI struct {
	t      *testing.T
	dsn    string
	direct bool
}

type roundCLIRequest struct {
	Repo       string `json:"repo"`
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Reason     string `json:"reason"`
	Batch      string `json:"batch"`
	Key        string `json:"key"`
	Detail     string `json:"detail"`
	Log        string `json:"log"`
}

type roundCLIMigration struct {
	ID            string     `json:"id"`
	Owner         string     `json:"owner"`
	Generation    int64      `json:"generation"`
	LandingRounds int        `json:"landing_rounds"`
	EscalatedAt   *time.Time `json:"escalated_at"`
}

type roundCLIEvent struct {
	Generation    int64  `json:"generation"`
	LandingRounds int    `json:"landing_rounds"`
	Reason        string `json:"reason"`
	Batch         string `json:"batch"`
	Key           string `json:"key"`
	Detail        string `json:"detail"`
	Log           string `json:"log"`
	ReadyCleared  bool   `json:"ready_cleared"`
}

func runRoundCLITests(t *testing.T, test func(*testing.T, roundCLI)) {
	t.Helper()
	for _, surface := range []struct {
		name   string
		direct bool
	}{
		{name: "flags", direct: true},
		{name: "request file"},
	} {
		t.Run(surface.name, func(t *testing.T) {
			cli := roundCLI{t: t, dsn: testpg.Database(t, "ledger_round_command"), direct: surface.direct}
			cli.invoke("repo", "ensure", "r", "3")
			cli.invoke("open", "r", "M-1", "alice", "base", "branch", "--write", "a.lean")
			test(t, cli)
		})
	}
}

func (cli roundCLI) call(args ...string) (int, string, string) {
	cli.t.Helper()
	var out, err bytes.Buffer
	code := run(context.Background(), args, func(key string) string {
		if key == "LEDGER_DSN" {
			return cli.dsn
		}
		return ""
	}, &out, &err)
	return code, out.String(), err.String()
}

func (cli roundCLI) invoke(args ...string) string {
	cli.t.Helper()
	code, stdout, stderr := cli.call(args...)
	if code != 0 {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	return stdout
}

func (cli roundCLI) roundArgs(request roundCLIRequest) []string {
	cli.t.Helper()
	if !cli.direct {
		contents, err := json.Marshal(request)
		if err != nil {
			cli.t.Fatal(err)
		}
		file := filepath.Join(cli.t.TempDir(), "round.json")
		if err := os.WriteFile(file, contents, 0o600); err != nil {
			cli.t.Fatal(err)
		}
		return []string{"--json", "round", "--request-file", file}
	}
	args := []string{"--json", "round", request.Repo, request.ID, fmt.Sprint(request.Generation), request.Reason, "--key", request.Key}
	for _, field := range []struct{ flag, value string }{
		{"--batch", request.Batch},
		{"--detail", request.Detail},
		{"--log", request.Log},
	} {
		if field.value != "" {
			args = append(args, field.flag, field.value)
		}
	}
	return args
}

func (cli roundCLI) round(request roundCLIRequest) roundCLIMigration {
	cli.t.Helper()
	var migration roundCLIMigration
	if err := json.Unmarshal([]byte(cli.invoke(cli.roundArgs(request)...)), &migration); err != nil {
		cli.t.Fatal(err)
	}
	return migration
}

func (cli roundCLI) events(kind string) []json.RawMessage {
	cli.t.Helper()
	var events []struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(cli.invoke("--json", "events", "r", "0", "M-1")), &events); err != nil {
		cli.t.Fatal(err)
	}
	var matches []json.RawMessage
	for _, event := range events {
		if event.Kind == kind {
			matches = append(matches, event.Data)
		}
	}
	return matches
}

func cliRoundRequest(generation int64, key, reason string) roundCLIRequest {
	return roundCLIRequest{
		Repo: "r", ID: "M-1", Generation: generation, Reason: reason, Batch: "batch-a", Key: key,
		Detail: "detail for " + key, Log: "https://logs.example.test/" + key,
	}
}

func TestRoundCommandRetryWithSameKeyCountsOnce(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		request := cliRoundRequest(1, "round-1", "build failed")
		first := cli.round(request)
		retry := cli.round(request)
		if first.LandingRounds != 1 || retry.LandingRounds != 1 {
			t.Fatalf("same key rounds: first=%+v retry=%+v", first, retry)
		}
		if events := cli.events("migration.round_failed"); len(events) != 1 {
			t.Fatalf("same key appended %d round events, want 1", len(events))
		}
	})
}

func TestRoundCommandRetryAfterTakeoverReturnsStoredResult(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		request := cliRoundRequest(1, "round-1", "build failed")
		cli.round(request)
		cli.invoke("takeover", "r", "M-1", "bob", "1")
		retry := cli.round(request)
		if retry.Generation != 2 || retry.Owner != "bob" || retry.LandingRounds != 1 {
			t.Fatalf("retry after takeover: %+v", retry)
		}
		if events := cli.events("migration.round_failed"); len(events) != 1 {
			t.Fatalf("retry after takeover appended %d round events, want 1", len(events))
		}
	})
}

func TestRoundCommandDifferentKeysCountTwice(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		cli.round(cliRoundRequest(1, "round-1", "build failed"))
		second := cli.round(cliRoundRequest(1, "round-2", "test failed"))
		if second.LandingRounds != 2 {
			t.Fatalf("two keys counted %d rounds, want 2", second.LandingRounds)
		}
		if events := cli.events("migration.round_failed"); len(events) != 2 {
			t.Fatalf("two keys appended %d round events, want 2", len(events))
		}
	})
}

func TestRoundCommandAtLimitEscalatesOnceAcrossRetry(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		cli.round(cliRoundRequest(1, "round-1", "build failed"))
		cli.round(cliRoundRequest(1, "round-2", "test failed"))
		third := cliRoundRequest(1, "round-3", "link failed")
		escalated := cli.round(third)
		retry := cli.round(third)
		if escalated.LandingRounds != 3 || escalated.EscalatedAt == nil || retry.LandingRounds != 3 || retry.EscalatedAt == nil {
			t.Fatalf("limit round: escalated=%+v retry=%+v", escalated, retry)
		}
		if events := cli.events("migration.escalated"); len(events) != 1 {
			t.Fatalf("limit round and retry appended %d escalation events, want 1", len(events))
		}
	})
}

func TestRoundCommandRefusesSameKeyWithDifferentBody(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*roundCLIRequest)
	}{
		{"generation", func(request *roundCLIRequest) { request.Generation = 2 }},
		{"reason", func(request *roundCLIRequest) { request.Reason = "test failed" }},
		{"batch", func(request *roundCLIRequest) { request.Batch = "batch-b" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
				request := cliRoundRequest(1, "round-1", "build failed")
				cli.round(request)
				if test.name == "generation" {
					cli.invoke("takeover", "r", "M-1", "bob", "1")
				}
				conflicting := request
				test.mutate(&conflicting)
				code, stdout, stderr := cli.call(cli.roundArgs(conflicting)...)
				if code != 1 || !strings.Contains(stdout+stderr, "round_key_conflict") {
					t.Fatalf("conflicting key: code=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if events := cli.events("migration.round_failed"); len(events) != 1 {
					t.Fatalf("conflicting key appended %d round events, want 1", len(events))
				}
			})
		})
	}
}

func TestRoundCommandEmptyKeyExitsTwo(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		code, stdout, stderr := cli.call(cli.roundArgs(cliRoundRequest(1, "", "build failed"))...)
		if code != 2 || !strings.Contains(stderr, "key") {
			t.Fatalf("empty key: code=%d stdout=%q stderr=%q; want exit 2 naming the key", code, stdout, stderr)
		}
		if events := cli.events("migration.round_failed"); len(events) != 0 {
			t.Fatalf("empty key appended %d round events", len(events))
		}
	})
}

func TestRoundCommandEventHoldsEveryField(t *testing.T) {
	runRoundCLITests(t, func(t *testing.T, cli roundCLI) {
		cli.invoke("ready", "r", "M-1", "1", "--head", "head-1")
		request := roundCLIRequest{
			Repo: "r", ID: "M-1", Generation: 1, Reason: "build failed", Batch: "batch-a", Key: "round-1",
			Detail: "compiler reported an error", Log: "https://logs.example.test/round-1",
		}
		cli.round(request)
		events := cli.events("migration.round_failed")
		if len(events) != 1 {
			t.Fatalf("round events %d, want 1", len(events))
		}
		var got roundCLIEvent
		if err := json.Unmarshal(events[0], &got); err != nil {
			t.Fatal(err)
		}
		want := roundCLIEvent{
			Generation: 1, LandingRounds: 1, Reason: "build failed", Batch: "batch-a", Key: "round-1",
			Detail: "compiler reported an error", Log: "https://logs.example.test/round-1", ReadyCleared: true,
		}
		if got != want {
			t.Fatalf("round event: got %+v, want %+v", got, want)
		}
	})
}
