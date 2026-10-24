package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
)

const cliEscalationOwner = "steward:landing"

// batchCLI runs the ledger command line against one database, as the landing program and an operator do.
type batchCLI struct {
	t   *testing.T
	dsn string
}

type cliBatch struct {
	Repo           string          `json:"repo"`
	ID             string          `json:"id"`
	Phase          string          `json:"phase"`
	Record         json.RawMessage `json:"record"`
	RecordVersion  int64           `json:"record_version"`
	Admission      string          `json:"admission"`
	Successor      string          `json:"successor"`
	PendingEffects []string        `json:"pending_effects"`
}

type cliCreated struct {
	Batch   cliBatch `json:"batch"`
	Created bool     `json:"created"`
}

func newBatchCLI(t *testing.T) batchCLI {
	t.Helper()
	cli := batchCLI{t: t, dsn: testpg.Database(t, "ledger_batches")}
	cli.invoke("repo", "ensure", "r", "3")
	return cli
}

func (cli batchCLI) call(args ...string) (int, string, string) {
	cli.t.Helper()
	return runLedger(cli.dsn, args...)
}

func runLedger(dsn string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, func(key string) string {
		if key == "LEDGER_DSN" {
			return dsn
		}
		return ""
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (cli batchCLI) invoke(args ...string) string {
	cli.t.Helper()
	code, stdout, stderr := cli.call(args...)
	if code != 0 {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
	}
	return stdout
}

func (cli batchCLI) decode(output string, value any) {
	cli.t.Helper()
	if err := json.Unmarshal([]byte(output), value); err != nil {
		cli.t.Fatalf("decode %q: %v", output, err)
	}
}

// writeRequestFile writes fields as a --request-file in dir and returns its path.
func writeRequestFile(t *testing.T, dir string, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(dir, "request-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func (cli batchCLI) request(fields map[string]any) string {
	cli.t.Helper()
	return writeRequestFile(cli.t, cli.t.TempDir(), fields)
}

func cliRecord(state string, fields ...any) map[string]any {
	record := map[string]any{"schema_version": 1, "state": state, "reason": "", "frozenMembers": []string{"M-1"}}
	for i := 0; i+1 < len(fields); i += 2 {
		record[fields[i].(string)] = fields[i+1]
	}
	return record
}

func unrecordedCLIRecord() map[string]any {
	return cliRecord("landed_unrecorded", "reason", "merged, but the ledger refused the landing", "escalate_to", cliEscalationOwner)
}

func (cli batchCLI) create(id string, record map[string]any) cliCreated {
	cli.t.Helper()
	var created cliCreated
	cli.decode(cli.invoke("create-batch", "--request-file", cli.request(map[string]any{"repo": "r", "id": id, "record": record})), &created)
	return created
}

func saveFields(id string, record any, version int64) map[string]any {
	return map[string]any{"repo": "r", "id": id, "record": record, "expected_version": version}
}

func (cli batchCLI) save(id string, record map[string]any, version int64) cliBatch {
	cli.t.Helper()
	var saved cliBatch
	cli.decode(cli.invoke("save-batch", "--request-file", cli.request(saveFields(id, record, version))), &saved)
	return saved
}

func (cli batchCLI) load(id string) cliBatch {
	cli.t.Helper()
	var batch cliBatch
	cli.decode(cli.invoke("batch", "r", id), &batch)
	return batch
}

func (cli batchCLI) active() []string {
	cli.t.Helper()
	var batches []cliBatch
	cli.decode(cli.invoke("batches", "r", "--active"), &batches)
	ids := []string{}
	for _, batch := range batches {
		ids = append(ids, batch.ID)
	}
	return ids
}

func (cli batchCLI) events(subject string) []string {
	cli.t.Helper()
	args := []string{"--json", "events", "r", "0"}
	if subject != "" {
		args = append(args, subject)
	}
	var events []struct {
		Kind string `json:"kind"`
	}
	cli.decode(cli.invoke(args...), &events)
	kinds := []string{}
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// sql runs statement on its own connection, for the columns that only later calls write (AdmitBatch, LandBatch,
// DeclineBatch and the effect log).
func (cli batchCLI) sql(statement string, args ...any) {
	cli.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, cli.dsn)
	if err != nil {
		cli.t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement, args...); err != nil {
		cli.t.Fatalf("%s: %v", statement, err)
	}
}

func (cli batchCLI) setAdmission(id, admission, successor string) {
	cli.t.Helper()
	cli.sql(`UPDATE batches SET admission = nullif($2, ''), successor = nullif($3, '') WHERE repo = 'r' AND id = $1`,
		id, admission, successor)
}

// assertUnchanged checks that the batch is stored as before and that the log has no new event.
func (cli batchCLI) assertUnchanged(before cliBatch, events int) {
	cli.t.Helper()
	if after := cli.load(before.ID); !reflect.DeepEqual(after, before) {
		cli.t.Fatalf("batch changed:\n got %+v\nwant %+v", after, before)
	}
	if got := len(cli.events("")); got != events {
		cli.t.Fatalf("%d events, want the %d there were", got, events)
	}
}

func sameRecord(t *testing.T, raw json.RawMessage, want map[string]any) bool {
	t.Helper()
	var got, expected any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &expected); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(got, expected)
}

// create-batch stores the record at version 1 and prints {batch, created}; a second create of the id prints the stored
// batch with created false. batch prints the stored batch, and save-batch the new one, with the record's state as the
// phase whatever it was.
func TestBatchCommandsCreateSaveAndLoad(t *testing.T) {
	cli := newBatchCLI(t)
	record := cliRecord("frozen", "candidate", map[string]any{"commit": "c1"})
	created := cli.create("batch-1", record)
	if !created.Created || created.Batch.Repo != "r" || created.Batch.Phase != "frozen" || created.Batch.RecordVersion != 1 ||
		created.Batch.PendingEffects == nil || !sameRecord(t, created.Batch.Record, record) {
		t.Fatalf("create-batch printed %+v", created)
	}
	if again := cli.create("batch-1", cliRecord("checked")); again.Created || !reflect.DeepEqual(again.Batch, created.Batch) {
		t.Fatalf("second create-batch printed %+v, want the stored %+v", again, created.Batch)
	}
	if loaded := cli.load("batch-1"); !reflect.DeepEqual(loaded, created.Batch) {
		t.Fatalf("batch printed %+v, want %+v", loaded, created.Batch)
	}
	saved := cli.save("batch-1", cliRecord("checked", "round", 1), 1)
	if saved.Phase != "checked" || saved.RecordVersion != 2 || !sameRecord(t, saved.Record, cliRecord("checked", "round", 1)) {
		t.Fatalf("save-batch printed %+v", saved)
	}
	if loaded := cli.load("batch-1"); !reflect.DeepEqual(loaded, saved) {
		t.Fatalf("batch after a save printed %+v, want %+v", loaded, saved)
	}
	cli.setAdmission("batch-1", "landed", "")
	if rewritten := cli.save("batch-1", cliRecord("failed"), 2); rewritten.Phase != "failed" || rewritten.Admission != "landed" {
		t.Fatalf("save-batch over a landed admission printed %+v", rewritten)
	}
	if got := cli.events("batch-1"); !slices.Equal(got, []string{"batch.created", "batch.saved", "batch.saved"}) {
		t.Fatalf("batch events = %q", got)
	}
	if code, stdout, stderr := cli.call("batch", "r", "batch-2"); code != 3 || !strings.Contains(stderr, "batch-2") {
		t.Fatalf("batch of an unknown id: code=%d stdout=%q stderr=%q, want exit 3", code, stdout, stderr)
	}
}

// save-batch from a version another writer has moved past exits 1 with stale_batch and changes nothing; after a
// reload it succeeds. A retry of a save whose response was lost prints the stored batch.
func TestSaveBatchCommandComparesTheRecordVersion(t *testing.T) {
	cli := newBatchCLI(t)
	cli.create("batch-1", cliRecord("frozen"))
	competing := cli.save("batch-1", cliRecord("checked"), 1)
	events := len(cli.events(""))
	code, stdout, stderr := cli.call("--json", "save-batch", "--request-file", cli.request(saveFields("batch-1", cliRecord("building"), 1)))
	if code != 1 || !strings.Contains(stdout, `"reason":"stale_batch"`) {
		t.Fatalf("stale save-batch: code=%d stdout=%q stderr=%q, want exit 1 with stale_batch", code, stdout, stderr)
	}
	cli.assertUnchanged(competing, events)
	reloaded := cli.save("batch-1", cliRecord("building"), cli.load("batch-1").RecordVersion)
	if reloaded.RecordVersion != 3 {
		t.Fatalf("save-batch after a reload printed %+v", reloaded)
	}
	events = len(cli.events(""))
	if retried := cli.save("batch-1", cliRecord("building"), 2); !reflect.DeepEqual(retried, reloaded) {
		t.Fatalf("retry of a save whose response was lost printed %+v, want the stored %+v", retried, reloaded)
	}
	cli.assertUnchanged(reloaded, events)
	if code, _, stderr := cli.call("save-batch", "--request-file", cli.request(saveFields("batch-2", cliRecord("checked"), 1))); code != 3 {
		t.Fatalf("save-batch of an unknown batch: code=%d stderr=%q, want exit 3", code, stderr)
	}
}

// A landed_unrecorded save through the command line enqueues landing:<id> for escalate_to. batch still shows the
// record and its reason, batches --active omits the batch, and a repeat of the save enqueues nothing new.
func TestSaveBatchCommandLandedUnrecordedEscalates(t *testing.T) {
	cli := newBatchCLI(t)
	cli.create("batch-1", cliRecord("publishing"))
	cli.setAdmission("batch-1", "admitted", "")
	saved := cli.save("batch-1", unrecordedCLIRecord(), 1)
	if saved.Phase != "landed_unrecorded" || saved.Admission != "admitted" || saved.RecordVersion != 2 {
		t.Fatalf("save-batch printed %+v", saved)
	}
	cli.assertEscalated(saved)
	events := len(cli.events(""))
	if replayed := cli.save("batch-1", unrecordedCLIRecord(), 1); !reflect.DeepEqual(replayed, saved) {
		t.Fatalf("repeat of the save printed %+v, want the stored %+v", replayed, saved)
	}
	cli.assertUnchanged(saved, events)
	if got := cli.events("landing:batch-1"); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q, want one item.enqueued", got)
	}
}

// assertEscalated checks that the landed_unrecorded batch is stored with its record and reason, that its escalation
// item is open for escalate_to, and that batches --active omits it.
func (cli batchCLI) assertEscalated(want cliBatch) {
	cli.t.Helper()
	var item struct {
		Owner string `json:"owner"`
		State string `json:"state"`
	}
	cli.decode(cli.invoke("item", "r", "landing:batch-1"), &item)
	if item.Owner != cliEscalationOwner || item.State != "open" {
		cli.t.Fatalf("escalation item = %+v", item)
	}
	shown := cli.load("batch-1")
	if !reflect.DeepEqual(shown, want) || !sameRecord(cli.t, shown.Record, unrecordedCLIRecord()) {
		cli.t.Fatalf("batch shows %+v, want %+v", shown, want)
	}
	if active := cli.active(); slices.Contains(active, "batch-1") {
		cli.t.Fatalf("batches --active lists the landed_unrecorded batch: %q", active)
	}
}

// saveCommandChildEnv names the database and request file of TestSaveBatchCommandCrashChild, joined by a newline.
// It is set only in the child process that TestSaveBatchCommandSurvivesAKillAfterTheCommit starts.
const saveCommandChildEnv = "LEDGER_TEST_SAVE_COMMAND_CHILD"

// TestSaveBatchCommandCrashChild is not a test of its own: it runs ledger save-batch in a child process, then kills
// that process with SIGKILL right after the command returns, before any cleanup.
func TestSaveBatchCommandCrashChild(t *testing.T) {
	dsn, file, ok := strings.Cut(os.Getenv(saveCommandChildEnv), "\n")
	if !ok {
		t.Skip("run only as the child process of TestSaveBatchCommandSurvivesAKillAfterTheCommit")
	}
	if code, _, _ := runLedger(dsn, "save-batch", "--request-file", file); code != 0 {
		os.Exit(code)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// A ledger process killed right after its landed_unrecorded save-batch commits leaves the batch and its escalation
// item, which a new ledger process finds, and a repeat of the save enqueues nothing new.
func TestSaveBatchCommandSurvivesAKillAfterTheCommit(t *testing.T) {
	cli := newBatchCLI(t)
	cli.create("batch-1", cliRecord("publishing"))
	cli.setAdmission("batch-1", "admitted", "")
	file := cli.request(saveFields("batch-1", unrecordedCLIRecord(), 1))
	child := exec.Command(os.Args[0], "-test.run=^TestSaveBatchCommandCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), saveCommandChildEnv+"="+cli.dsn+"\n"+file)
	err := child.Run()
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended with %v (%v), want SIGKILL", child.ProcessState, err)
	}

	stored := cli.load("batch-1")
	if stored.Phase != "landed_unrecorded" || stored.RecordVersion != 2 {
		t.Fatalf("after the kill the batch is %+v", stored)
	}
	cli.assertEscalated(stored)
	events := len(cli.events(""))
	if replayed := cli.save("batch-1", unrecordedCLIRecord(), 1); !reflect.DeepEqual(replayed, stored) {
		t.Fatalf("repeat of the save printed %+v, want the stored %+v", replayed, stored)
	}
	cli.assertUnchanged(stored, events)
	if got := cli.events("landing:batch-1"); !slices.Equal(got, []string{"item.enqueued"}) {
		t.Fatalf("escalation item events = %q, want one item.enqueued", got)
	}
}

// batches <repo> --active lists, by creation, the active batches, a declined batch whose successor is missing and a
// terminal batch with an effect still to do, with its pending effect keys. A declined batch whose successor is stored
// as the empty string names no successor, so it is not listed.
func TestBatchesActiveCommand(t *testing.T) {
	cli := newBatchCLI(t)
	for _, id := range []string{"batch-b", "batch-a"} {
		cli.create(id, cliRecord("frozen"))
	}
	cli.create("failed", cliRecord("failed"))
	for _, end := range []struct{ id, phase, admission, successor string }{
		{"landed", "landed", "landed", ""},
		{"declined", "declined", "declined", "successor"},
		{"declined-empty-successor", "declined", "declined", ""},
		{"effects", "landed", "landed", ""},
	} {
		cli.create(end.id, cliRecord(end.phase))
		cli.setAdmission(end.id, end.admission, end.successor)
	}
	cli.sql(`UPDATE batches SET successor = '' WHERE repo = 'r' AND id = 'declined-empty-successor'`)
	// LandBatch records a landed batch's effects in a later task; the test writes one directly.
	cli.sql(`INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'effects', 'hook/effects/0/report', 'hook')`)
	var batches []cliBatch
	cli.decode(cli.invoke("batches", "r", "--active"), &batches)
	ids, pending := []string{}, map[string][]string{}
	for _, batch := range batches {
		ids = append(ids, batch.ID)
		pending[batch.ID] = batch.PendingEffects
	}
	if want := []string{"batch-b", "batch-a", "declined", "effects"}; !slices.Equal(ids, want) {
		t.Fatalf("batches --active = %q, want %q", ids, want)
	}
	if !slices.Equal(pending["effects"], []string{"hook/effects/0/report"}) || pending["batch-b"] == nil || len(pending["batch-b"]) != 0 {
		t.Fatalf("pending effects = %q", pending)
	}
	cli.create("successor", cliRecord("frozen"))
	if want := []string{"batch-b", "batch-a", "effects", "successor"}; !slices.Equal(cli.active(), want) {
		t.Fatalf("batches --active after the successor = %q, want %q", cli.active(), want)
	}
	if code, _, stderr := cli.call("batches", "missing", "--active"); code != 3 {
		t.Fatalf("batches of a missing repository: code=%d stderr=%q, want exit 3", code, stderr)
	}
}

// A malformed batch command exits 2 and changes nothing.
func TestBatchCommandsRefuseMalformedInput(t *testing.T) {
	cli := newBatchCLI(t)
	cli.create("batch-1", cliRecord("frozen"))
	before := cli.load("batch-1")
	events := len(cli.events(""))
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, test := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"create-batch without a request file", []string{"create-batch", "r", "batch-2"}, "create-batch --request-file"},
		{"create-batch with an unknown field", []string{"create-batch", "--request-file", write(`{"repo":"r","id":"batch-2","record":{"state":"frozen"},"expected_version":1}`)}, "unknown field"},
		{"create-batch with two values", []string{"create-batch", "--request-file", write(`{"repo":"r","id":"batch-2","record":{"state":"frozen"}} {}`)}, "one JSON value"},
		{"save-batch without a request file", []string{"save-batch", "r", "batch-1"}, "save-batch --request-file"},
		{"save-batch with an unknown field", []string{"save-batch", "--request-file", write(`{"repo":"r","id":"batch-1","record":{"state":"checked"},"expected_version":1,"admission":"admitted"}`)}, "unknown field"},
		{"batch with too few arguments", []string{"batch", "r"}, "batch <repo> <id>"},
		{"batches without --active", []string{"batches", "r"}, "batches <repo> --active"},
		{"batches with another flag", []string{"batches", "r", "--all"}, "batches <repo> --active"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := cli.call(test.args...)
			if code != 2 || !strings.Contains(stderr, test.stderr) {
				t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
			}
		})
	}
	cli.assertUnchanged(before, events)
}
