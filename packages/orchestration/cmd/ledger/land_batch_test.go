package main

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cliEffectRow is one effect as the batch_effects table holds it, with its result.
type cliEffectRow struct {
	Key, Member, Op, State string
	Result                 map[string]any
}

// effectRows reads the effects of batch id, by key, from the table that the landing commands write.
func (cli batchCLI) effectRows(id string) []cliEffectRow {
	cli.t.Helper()
	var rows []string
	cli.queryRows(&rows, `SELECT json_build_object('Key', key, 'Member', member, 'Op', op, 'State', state, 'Result', result)::text
		FROM batch_effects WHERE repo = 'r' AND batch = $1 ORDER BY key`, id)
	effects := []cliEffectRow{}
	for _, row := range rows {
		var effect cliEffectRow
		cli.decode(row, &effect)
		effects = append(effects, effect)
	}
	return effects
}

// migration prints migration id's state and landed commit.
func (cli batchCLI) migration(id string) (string, string) {
	cli.t.Helper()
	var rows []string
	cli.queryRows(&rows, `SELECT state || ' ' || coalesce(landed_commit, '') FROM migrations WHERE repo = 'r' AND id = $1`, id)
	if len(rows) != 1 {
		cli.t.Fatalf("migration %s: %q", id, rows)
	}
	state, commit, _ := strings.Cut(rows[0], " ")
	return state, commit
}

// landRecordFields is the landed record of batch id at mergedCommit, with one post-landing hook of the whole batch.
func landRecordFields(id, mergedCommit string) map[string]any {
	return cliRecord("landed", "pull_request", map[string]any{"number": 7, "merged_commit": mergedCommit},
		"post_land_effects", []map[string]any{{"key": "hook/" + id + "/0/report", "op": "hook"}})
}

func landFields(id string, version int64, mergedCommit string) map[string]any {
	return map[string]any{"repo": "r", "id": id, "merged_commit": mergedCommit, "expected_version": version,
		"record": landRecordFields(id, mergedCommit)}
}

func (cli batchCLI) landBatch(fields map[string]any) cliBatch {
	cli.t.Helper()
	var landed cliBatch
	cli.decode(cli.invoke("land-batch", "--request-file", cli.request(fields)), &landed)
	return landed
}

// admittedCLIBatch admits batch-1 with M-1, which changes contract C with its owner's review, and M-2, and returns it.
func admittedCLIBatch(t *testing.T) (batchCLI, cliBatch) {
	t.Helper()
	cli := newBatchCLI(t)
	cli.invoke("contract", "register", "r", "C", "alice", "c1", "false")
	changer := cli.openReady("M-1", "alice", "a.lean", map[string]string{"C": "c1"}, "--changed-contract", "C=c2", "--review", "C=alice@head-M-1")
	other := cli.openReady("M-2", "bob", "b.lean", nil)
	cli.create("batch-1", cliRecord("checked"))
	return cli, cli.admit(admitFields("batch-1", 1, changer, other))
}

// land-batch lands every member at the merged commit and prints the landed batch at its new version, with the merged
// commit and the post-landing effect still to do; the effect's intent holds the merged commit. A retry with the same
// merged commit prints the stored batch and changes nothing; save-batch from the old version is refused stale_batch.
// After the landing, the contract a member changed can be transferred, and a takeover of a member is refused not_open.
func TestLandBatchCommandLandsTheAdmittedMembers(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	request := landFields("batch-1", admitted.RecordVersion, "merged-1")
	landed := cli.landBatch(request)
	if landed.Phase != "landed" || landed.Admission != "landed" || landed.MergedCommit != "merged-1" ||
		landed.RecordVersion != admitted.RecordVersion+1 || !slices.Equal(landed.PendingEffects, []string{"hook/batch-1/0/report"}) ||
		!sameRecord(t, landed.Record, landRecordFields("batch-1", "merged-1")) {
		t.Fatalf("land-batch printed %+v", landed)
	}
	for _, id := range []string{"M-1", "M-2"} {
		if state, commit := cli.migration(id); state != "landed" || commit != "merged-1" {
			t.Fatalf("migration %s after land-batch: %s at %q", id, state, commit)
		}
	}
	if members := cli.members("batch-1"); !slices.Equal(members, []string{"M-1:false", "M-2:false"}) {
		t.Fatalf("members after land-batch = %q", members)
	}
	want := []cliEffectRow{{Key: "hook/batch-1/0/report", Op: "hook", State: "intent", Result: map[string]any{"commit": "merged-1"}}}
	if effects := cli.effectRows("batch-1"); !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects = %+v, want %+v", effects, want)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active = %q, want batch-1 while its effect is pending", active)
	}

	events := len(cli.events(""))
	retry := landFields("batch-1", admitted.RecordVersion, "merged-1")
	retry["record"] = cliRecord("other")
	if replayed := cli.landBatch(retry); !reflect.DeepEqual(replayed, landed) {
		t.Fatalf("retry printed %+v, want the stored %+v", replayed, landed)
	}
	cli.assertUnchanged(landed, events)
	cli.refused("stale_batch", "save-batch", "--request-file", cli.request(saveFields("batch-1", cliRecord("landed"), admitted.RecordVersion)))
	cli.assertUnchanged(landed, events)

	cli.invoke("contract", "transfer", "r", "C", "alice", "bob")
	cli.refused("not_open", "takeover", "r", "M-1", "zoe", "1")
}

// A land-batch from a version that a competing save moved past exits 1 with stale_batch and changes nothing.
func TestLandBatchCommandComparesTheRecordVersion(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	competing := cli.save("batch-1", cliRecord("publishing", "round", 2), admitted.RecordVersion)
	events := len(cli.events(""))
	cli.refused("stale_batch", "land-batch", "--request-file", cli.request(landFields("batch-1", admitted.RecordVersion, "merged-1")))
	cli.assertUnchanged(competing, events)
	if state, _ := cli.migration("M-1"); state != "open" {
		t.Fatalf("a refused land-batch closed M-1: %s", state)
	}
}

// landCommandChildEnv names the database and request file of TestLandBatchCommandCrashChild, joined by a newline.
const landCommandChildEnv = "LEDGER_TEST_LAND_COMMAND_CHILD"

// TestLandBatchCommandCrashChild is not a test of its own: it runs ledger land-batch in a child process, then kills
// that process with SIGKILL right after the command returns, before any cleanup.
func TestLandBatchCommandCrashChild(t *testing.T) {
	dsn, file, ok := strings.Cut(os.Getenv(landCommandChildEnv), "\n")
	if !ok {
		t.Skip("run only as the child process of TestLandBatchCommandSurvivesAKillAfterTheCommit")
	}
	if code, _, _ := runLedger(dsn, "land-batch", "--request-file", file); code != 0 {
		os.Exit(code)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// A ledger process killed right after its land-batch commits leaves the landing and its effect intent, which a new
// process finds, and a repeat of the request prints the stored batch and changes nothing.
func TestLandBatchCommandSurvivesAKillAfterTheCommit(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	file := cli.request(landFields("batch-1", admitted.RecordVersion, "merged-1"))
	child := exec.Command(os.Args[0], "-test.run=^TestLandBatchCommandCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), landCommandChildEnv+"="+cli.dsn+"\n"+file)
	err := child.Run()
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended with %v (%v), want SIGKILL", child.ProcessState, err)
	}

	var stored cliBatch
	cli.decode(cli.invoke("batch", "r", "batch-1"), &stored)
	if stored.Admission != "landed" || stored.MergedCommit != "merged-1" || len(cli.effectRows("batch-1")) != 1 {
		t.Fatalf("after the kill the batch is %+v", stored)
	}
	events := len(cli.events(""))
	var replayed cliBatch
	cli.decode(cli.invoke("land-batch", "--request-file", file), &replayed)
	if !reflect.DeepEqual(replayed, stored) {
		t.Fatalf("the repeat printed %+v, want the stored %+v", replayed, stored)
	}
	cli.assertUnchanged(stored, events)
}

// A malformed land-batch exits 2 and changes nothing.
func TestLandBatchCommandRefusesMalformedInput(t *testing.T) {
	cli, admitted := admittedCLIBatch(t)
	events := len(cli.events(""))
	path := cli.request(map[string]any{"repo": "r", "id": "batch-1", "merged_commit": "m", "expected_version": 2, "record": map[string]any{}, "members": []string{}})
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"land-batch", "r", "batch-1"}, "land-batch --request-file"},
		{[]string{"land-batch", "--request-file", path}, "unknown field"},
	} {
		if code, stdout, stderr := cli.call(test.args...); code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
		}
	}
	cli.assertUnchanged(admitted, events)
}

// queryRows reads one text column of every row that statement returns into rows.
func (cli batchCLI) queryRows(rows *[]string, statement string, args ...any) {
	cli.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, cli.dsn)
	if err != nil {
		cli.t.Fatal(err)
	}
	defer conn.Close(ctx)
	result, err := conn.Query(ctx, statement, args...)
	if err != nil {
		cli.t.Fatal(err)
	}
	if *rows, err = pgx.CollectRows(result, pgx.RowTo[string]); err != nil {
		cli.t.Fatal(err)
	}
}
