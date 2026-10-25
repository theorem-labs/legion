package ledger_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// batchCrashChildEnv names the database and the call of TestBatchCrashChild, joined by a newline. It is set only in
// the child process that the batch crash tests start.
const batchCrashChildEnv = "LEDGER_TEST_BATCH_CRASH_CHILD"

// holdLock is the advisory lock that the hold trigger waits on, so that a test can stop a call at its last write.
const holdLock = 730000002

// batchCrash is one batch call that must commit all of its writes or none.
type batchCrash struct {
	call string
	// fixture builds the ledger the call starts from.
	fixture func(t *testing.T) (*ledger.Store, string)
	// run makes the call, in the child process and in the parent's repeat.
	run func(s *ledger.Store) (ledger.Batch, error)
	// last is the kind of the call's last event, which it appends after every other write.
	last string
	// written are tables the call has written by its last event.
	written []string
	// state reads everything the call writes.
	state func(t *testing.T, s *ledger.Store, dsn string) landingState
	// committed checks what the call's commit leaves.
	committed func(t *testing.T, s *ledger.Store, dsn string, batch ledger.Batch)
}

func batchCrashes() []batchCrash {
	ctx := context.Background()
	return []batchCrash{{
		call: "land",
		fixture: func(t *testing.T) (*ledger.Store, string) {
			f := admittedLanding(t)
			return f.s, f.dsn
		},
		run: func(s *ledger.Store) (ledger.Batch, error) {
			return s.LandBatch(ctx, landBatchRequest("batch-1", 2, "merged-1"))
		},
		last:    "batch.landed",
		written: []string{"migrations", "contracts", "batch_members", "batches", "batch_effects"},
		state: func(t *testing.T, s *ledger.Store, dsn string) landingState {
			return readLandingState(t, s, dsn, "batch-1", "M-1", "M-2")
		},
		committed: func(t *testing.T, s *ledger.Store, dsn string, batch ledger.Batch) {
			if batch.Admission != "landed" || batch.MergedCommit != "merged-1" || len(batch.PendingEffects) != 2 {
				t.Fatalf("the batch after the landing is %+v", batch)
			}
			if m := must(s.Migration(ctx, "r", "M-1")); m.State != ledger.StateLanded || m.LandedCommit != "merged-1" {
				t.Fatalf("M-1 after the landing is %+v", m)
			}
		},
	}, {
		call: "resolve",
		fixture: func(t *testing.T) (*ledger.Store, string) {
			f := unrecordedLanding(t, map[string]any{"number": 7})
			return f.s, f.dsn
		},
		run: func(s *ledger.Store) (ledger.Batch, error) {
			return s.ResolveBatch(ctx, "r", "batch-1", "merged-9")
		},
		last:    "batch.resolved",
		written: []string{"migrations", "contracts", "batch_members", "batches", "batch_effects"},
		state: func(t *testing.T, s *ledger.Store, dsn string) landingState {
			return readLandingState(t, s, dsn, "batch-1", "M-1", "M-2")
		},
		committed: func(t *testing.T, s *ledger.Store, dsn string, batch ledger.Batch) {
			if batch.Phase != ledger.PhaseLanded || batch.MergedCommit != "merged-9" || len(batch.PendingEffects) != 2 {
				t.Fatalf("the batch after the resolution is %+v", batch)
			}
			if m := must(s.Migration(ctx, "r", "M-2")); m.State != ledger.StateLanded || m.LandedCommit != "merged-9" {
				t.Fatalf("M-2 after the resolution is %+v", m)
			}
		},
	}}
}

// TestBatchCrashChild is not a test of its own: the batch crash tests run it in a child process. It makes one batch
// call, then kills its own process with SIGKILL, so the process ends with no cleanup right after the call's commit, or
// wherever the parent kills it first.
func TestBatchCrashChild(t *testing.T) {
	dsn, call, ok := strings.Cut(os.Getenv(batchCrashChildEnv), "\n")
	if !ok {
		t.Skip("run only as the child process of the batch crash tests")
	}
	s, err := ledger.Open(context.Background(), dsn)
	if err != nil {
		os.Exit(2)
	}
	for _, crash := range batchCrashes() {
		if crash.call == call {
			if _, err := crash.run(s); err != nil {
				os.Exit(3)
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	os.Exit(4)
}

// startBatchCrashChild starts TestBatchCrashChild with call against dsn in a new process, whose connections carry
// application_name crash_child.
func startBatchCrashChild(t *testing.T, dsn, call string) *exec.Cmd {
	t.Helper()
	childDSN := must(url.Parse(dsn))
	query := childDSN.Query()
	query.Set("application_name", "crash_child")
	childDSN.RawQuery = query.Encode()
	child := exec.Command(os.Args[0], "-test.run=^TestBatchCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), batchCrashChildEnv+"="+childDSN.String()+"\n"+call)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	return child
}

// A process killed right after a batch call commits leaves every write of the call, which a new process finds; a repeat
// of the call from that process returns the stored batch and changes nothing.
func TestBatchCallsSurviveAKillAfterTheCommit(t *testing.T) {
	ctx := context.Background()
	for _, crash := range batchCrashes() {
		t.Run(crash.call, func(t *testing.T) {
			s, dsn := crash.fixture(t)
			waitForKill(t, startBatchCrashChild(t, dsn, crash.call))
			s.Close()

			restarted := must(ledger.Open(ctx, dsn))
			defer restarted.Close()
			batch := must(restarted.LoadBatch(ctx, "r", "batch-1"))
			crash.committed(t, restarted, dsn, batch)
			before := crash.state(t, restarted, dsn)
			if repeated := must(crash.run(restarted)); !reflect.DeepEqual(repeated, batch) {
				t.Fatalf("the repeat returned %+v, want the stored %+v", repeated, batch)
			}
			if after := crash.state(t, restarted, dsn); !reflect.DeepEqual(after, before) {
				t.Fatalf("the repeat changed the ledger:\n got %+v\nwant %+v", after, before)
			}
		})
	}
}

// A process killed inside a batch call, after its every other write and while its last event waits, leaves none of
// them: the ledger is as it was, and the call then succeeds. A trigger that this test adds makes the call's last event
// wait on an advisory lock that the test holds.
func TestBatchCallsKilledBeforeTheCommitLeaveNothing(t *testing.T) {
	ctx := context.Background()
	for _, crash := range batchCrashes() {
		t.Run(crash.call, func(t *testing.T) {
			s, dsn := crash.fixture(t)
			before := crash.state(t, s, dsn)
			conn := must(pgx.Connect(ctx, dsn))
			defer conn.Close(ctx)
			must(conn.Exec(ctx, `CREATE FUNCTION hold_last_event() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.kind = TG_ARGV[0] THEN
						PERFORM pg_advisory_xact_lock(`+strconv.Itoa(holdLock)+`);
					END IF;
					RETURN NEW;
				END
			$$`))
			must(conn.Exec(ctx, `CREATE TRIGGER hold_last_event AFTER INSERT ON events
				FOR EACH ROW EXECUTE FUNCTION hold_last_event('`+crash.last+`')`))
			must(conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, holdLock))

			child := startBatchCrashChild(t, dsn, crash.call)
			watcher := must(pgx.Connect(ctx, dsn))
			defer watcher.Close(ctx)
			waitForBlockedConnections(t, watcher, 1)
			for _, table := range crash.written {
				var written bool
				if err := watcher.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
					WHERE a.application_name = 'crash_child' AND l.locktype = 'relation' AND l.database = a.datid
						AND l.relation = $1::regclass AND l.mode = 'RowExclusiveLock' AND l.granted)`, table).
					Scan(&written); err != nil || !written {
					t.Fatalf("the child holds no RowExclusiveLock on %s while it waits (%v): it has not written it", table, err)
				}
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitForKill(t, child)
			// Only the dead client could send the COMMIT, so the backend that the unlock lets go rolls back.
			must(conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, holdLock))
			waitForNoBackends(t, watcher, "crash_child")

			if after := crash.state(t, s, dsn); !reflect.DeepEqual(after, before) {
				t.Fatalf("a kill inside the call left writes:\n got %+v\nwant %+v", after, before)
			}
			crash.committed(t, s, dsn, must(crash.run(s)))
		})
	}
}
