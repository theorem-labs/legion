package ledger_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// effectCrashChildEnv names the database and the call of TestEffectCrashChild, joined by a newline. It is set only in
// the child process that the effect crash tests start.
const effectCrashChildEnv = "LEDGER_TEST_EFFECT_CRASH_CHILD"

// The calls the child makes, and the requests they send.
const (
	crashDoneThen     = "done-then"
	crashDoneEscalate = "done-escalate"
	crashAttempt      = "attempt"
	crashApply        = "apply"
)

var (
	crashOps       = opsLines(registerOp("New", "owner:alice", "h-new"), librarianOp("near-1"), registerOp("Same", "owner:bob", "h-same"), librarianOp("near-2"))
	crashReport    = []byte("report\x00bytes\n")
	crashFollowOns = []ledger.FollowOn{
		{Key: applyKey, Op: "apply", Kind: "contracts", Input: "ops.jsonl"},
		{Key: "publish/batch-1/0/report", Op: "publish"},
	}
)

func crashDoneRequest() ledger.DoneRequest {
	return ledger.DoneRequest{
		Result:  jsonValue(map[string]any{"outcome": "exit", "exit": 0}),
		Then:    crashFollowOns,
		Outputs: []ledger.OutputFile{{Name: "ops.jsonl", Bytes: crashOps}, {Name: "report.txt", Bytes: crashReport}},
	}
}

func crashEscalateRequest() ledger.DoneRequest {
	return ledger.DoneRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 1}), EscalateTo: escalationOwner}
}

func crashAttemptRequest() ledger.AttemptRequest {
	return ledger.AttemptRequest{Result: jsonValue(map[string]any{"outcome": "exit", "exit": 75}), EscalateTo: escalationOwner, EscalateAfter: 3}
}

// TestEffectCrashChild is not a test of its own: the effect crash tests run it in a child process. It makes one
// effect call on batch-1 of repository r, then kills its own process with SIGKILL, so the process ends with no
// cleanup right after the call's commit, or wherever the parent kills it first.
func TestEffectCrashChild(t *testing.T) {
	dsn, call, ok := strings.Cut(os.Getenv(effectCrashChildEnv), "\n")
	if !ok {
		t.Skip("run only as the child process of the effect crash tests")
	}
	ctx := context.Background()
	s, err := ledger.Open(ctx, dsn)
	if err != nil {
		os.Exit(2)
	}
	switch call {
	case crashDoneThen:
		_, err = s.BatchEffectDone(ctx, "r", "batch-1", hookKey, crashDoneRequest())
	case crashDoneEscalate:
		_, err = s.BatchEffectDone(ctx, "r", "batch-1", hookKey, crashEscalateRequest())
	case crashAttempt:
		_, err = s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, crashAttemptRequest())
	case crashApply:
		_, err = s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
	default:
		os.Exit(4)
	}
	if err != nil {
		os.Exit(3)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// startEffectCrashChild starts TestEffectCrashChild with call against dsn in a new process, whose connections carry
// application_name crash_child.
func startEffectCrashChild(t *testing.T, dsn, call string) *exec.Cmd {
	t.Helper()
	childDSN := must(url.Parse(dsn))
	query := childDSN.Query()
	query.Set("application_name", "crash_child")
	childDSN.RawQuery = query.Encode()
	child := exec.Command(os.Args[0], "-test.run=^TestEffectCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), effectCrashChildEnv+"="+childDSN.String()+"\n"+call)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	return child
}

// effectCrash is one effect call that must commit all of its writes or none.
type effectCrash struct {
	name    string
	call    string
	fixture func(t *testing.T) (*ledger.Store, string)
	// hold is an uncommitted insert that the call's last row write waits on, and written a table that the call has
	// written by then, so a kill while it waits is a kill inside its transaction, after its first writes.
	hold, written string
	// committed checks what the call's commit leaves, in a new process.
	committed func(t *testing.T, s *ledger.Store, dsn string)
	// repeat, when set, is the same call again, which must change nothing.
	repeat func(s *ledger.Store) error
}

func effectCrashes() []effectCrash {
	ctx := context.Background()
	return []effectCrash{{
		name:    "a done with follow-ons and outputs",
		call:    crashDoneThen,
		fixture: effectFixture,
		hold:    `INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'batch-1', '` + applyKey + `', 'hold')`,
		written: "batch_effect_outputs",
		committed: func(t *testing.T, s *ledger.Store, _ string) {
			if done := effectOf(t, s, "batch-1", hookKey); done.State != ledger.EffectDone {
				t.Fatalf("after a kill after the commit the hook is %+v", done)
			}
			outputs := must(s.BatchEffectOutputs(ctx, "r", "batch-1", hookKey))
			if len(outputs) != 2 || string(outputs[0].Bytes) != string(crashOps) || outputs[0].SHA256 != sha256Hex(crashOps) ||
				string(outputs[1].Bytes) != string(crashReport) || outputs[1].SHA256 != sha256Hex(crashReport) {
				t.Fatalf("after a kill after the commit the outputs are %+v", outputs)
			}
			for _, follow := range crashFollowOns {
				if effect := effectOf(t, s, "batch-1", follow.Key); effect.State != ledger.EffectIntent ||
					!sameJSON(t, effect.Result, jsonValue(map[string]any{"commit": landingCommit, "hook": hookKey, "input": follow.Input})) {
					t.Fatalf("after a kill after the commit the follow-on %s is %+v", follow.Key, effect)
				}
			}
		},
		repeat: func(s *ledger.Store) error {
			_, err := s.BatchEffectDone(ctx, "r", "batch-1", hookKey, crashDoneRequest())
			return err
		},
	}, {
		name:    "a done with escalate_to",
		call:    crashDoneEscalate,
		fixture: effectFixture,
		hold:    `INSERT INTO work_items (repo, id, owner) VALUES ('r', 'landing:batch-1:` + hookKey + `', 'hold')`,
		written: "batch_effects",
		committed: func(t *testing.T, s *ledger.Store, dsn string) {
			if done := effectOf(t, s, "batch-1", hookKey); done.State != ledger.EffectDone {
				t.Fatalf("after a kill after the commit the hook is %+v", done)
			}
			if got := workItems(t, dsn); !slices.Equal(got, []string{"landing:batch-1:" + hookKey + " " + escalationOwner + " open"}) {
				t.Fatalf("after a kill after the commit the work items are %q", got)
			}
		},
		repeat: func(s *ledger.Store) error {
			_, err := s.BatchEffectDone(ctx, "r", "batch-1", hookKey, crashEscalateRequest())
			return err
		},
	}, {
		name: "the attempt that reaches the threshold",
		call: crashAttempt,
		fixture: func(t *testing.T) (*ledger.Store, string) {
			s, dsn := effectFixture(t)
			for range 2 {
				must(s.BatchEffectAttempt(ctx, "r", "batch-1", hookKey, crashAttemptRequest()))
			}
			return s, dsn
		},
		hold:    `INSERT INTO work_items (repo, id, owner) VALUES ('r', 'landing:batch-1:` + hookKey + `@3', 'hold')`,
		written: "batch_effects",
		committed: func(t *testing.T, s *ledger.Store, dsn string) {
			item := "landing:batch-1:" + hookKey + "@3"
			if effect := effectOf(t, s, "batch-1", hookKey); effect.Attempts != 3 || effect.State != ledger.EffectIntent {
				t.Fatalf("after a kill after the commit the effect is %+v", effect)
			}
			if got := workItems(t, dsn); !slices.Equal(got, []string{item + " " + escalationOwner + " open"}) {
				t.Fatalf("after a kill after the commit the work items are %q", got)
			}
			if got := eventKinds(t, s, item); !slices.Equal(got, []string{"item.enqueued"}) {
				t.Fatalf("escalation item events = %q", got)
			}
		},
	}, {
		name: "a contracts apply",
		call: crashApply,
		fixture: func(t *testing.T) (*ledger.Store, string) {
			s, dsn := applyFixture(t, crashOps)
			must(s.RegisterContract(ctx, "r", "Same", "owner:bob", "h-same", false))
			return s, dsn
		},
		hold:    `INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'batch-1', 'librarian/batch-1/near-1', 'hold')`,
		written: "contracts",
		committed: func(t *testing.T, s *ledger.Store, _ string) {
			if apply := effectOf(t, s, "batch-1", applyKey); apply.State != ledger.EffectDone {
				t.Fatalf("after a kill after the commit the apply is %+v", apply)
			}
			if c := must(s.Contract(ctx, "r", "New")); c.Owner != "owner:alice" || c.StatementHash != "h-new" {
				t.Fatalf("after a kill after the commit the registered contract is %+v", c)
			}
			for _, id := range []string{"near-1", "near-2"} {
				if intent := effectOf(t, s, "batch-1", "librarian/batch-1/"+id); intent.State != ledger.EffectIntent {
					t.Fatalf("after a kill after the commit the librarian intent %s is %+v", id, intent)
				}
			}
		},
		// A repeat of a done apply is refused with effect_done and changes nothing.
		repeat: func(s *ledger.Store) error {
			_, err := s.BatchEffectApplyContracts(ctx, "r", "batch-1", applyKey)
			if refusal := (*ledger.Refusal)(nil); errors.As(err, &refusal) && refusal.Reason == ledger.ReasonEffectDone {
				return nil
			}
			return err
		},
	}}
}

// A process killed right after an effect call commits leaves every write of the call, which a new process finds, and
// a repeat of the call from that process changes nothing.
func TestEffectCallsSurviveAKillAfterTheCommit(t *testing.T) {
	for _, test := range effectCrashes() {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := test.fixture(t)
			waitForKill(t, startEffectCrashChild(t, dsn, test.call))
			s.Close()

			restarted := must(ledger.Open(ctx, dsn))
			defer restarted.Close()
			test.committed(t, restarted, dsn)
			if test.repeat == nil {
				return
			}
			before := snapshot(t, restarted, dsn, "batch-1")
			if err := test.repeat(restarted); err != nil {
				t.Fatalf("the repeat after the kill: %v", err)
			}
			assertLedgerUnchanged(t, restarted, dsn, before)
		})
	}
}

// A process killed inside an effect call's transaction, after its first writes and while its last one waits, leaves
// none of them: the ledger is as it was, and the call succeeds afterwards. The test holds an uncommitted insert that
// the call's last write waits on; then it kills the child and rolls its insert back.
func TestEffectCallsKilledInsideTheTransactionLeaveNothing(t *testing.T) {
	for _, test := range effectCrashes() {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := test.fixture(t)
			before := snapshot(t, s, dsn, "batch-1")
			conn := must(pgx.Connect(ctx, dsn))
			defer conn.Close(ctx)
			holder := must(conn.Begin(ctx))
			must(holder.Exec(ctx, test.hold))

			child := startEffectCrashChild(t, dsn, test.call)
			watcher := must(pgx.Connect(ctx, dsn))
			defer watcher.Close(ctx)
			waitForBlockedConnections(t, watcher, 1)
			var written bool
			if err := watcher.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
				WHERE a.application_name = 'crash_child' AND l.locktype = 'relation' AND l.database = a.datid
					AND l.relation = $1::regclass AND l.mode = 'RowExclusiveLock' AND l.granted)`, test.written).
				Scan(&written); err != nil || !written {
				t.Fatalf("the child holds no RowExclusiveLock on %s while it waits (%v): it has not written it", test.written, err)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitForKill(t, child)
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			waitForNoBackends(t, watcher, "crash_child")

			assertLedgerUnchanged(t, s, dsn, before)
			waitForKill(t, startEffectCrashChild(t, dsn, test.call))
			test.committed(t, s, dsn)
		})
	}
}
