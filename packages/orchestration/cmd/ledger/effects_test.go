package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (cli batchCLI) ctx() context.Context { return context.Background() }

// connect opens a connection of its own to the test's database.
func (cli batchCLI) connect() *pgx.Conn {
	cli.t.Helper()
	conn, err := pgx.Connect(cli.ctx(), cli.dsn)
	if err != nil {
		cli.t.Fatal(err)
	}
	return conn
}

// waitForChildLock waits until a session named crash_child waits on a lock while it holds RowExclusiveLock on table,
// so it has written that table inside its open transaction.
func (cli batchCLI) waitForChildLock(watcher *pgx.Conn, table string) {
	cli.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ready bool
		if err := watcher.QueryRow(cli.ctx(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity a
			WHERE a.application_name = 'crash_child' AND a.datname = current_database() AND a.wait_event_type = 'Lock'
				AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid = a.pid AND l.locktype = 'relation'
					AND l.relation = $1::regclass AND l.mode = 'RowExclusiveLock' AND l.granted))`, table).Scan(&ready); err != nil {
			cli.t.Fatal(err)
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			cli.t.Fatalf("no crash_child session waits on a lock after writing %s", table)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForNoChild waits until no session named crash_child remains, so the killed child's transaction has ended.
func (cli batchCLI) waitForNoChild(watcher *pgx.Conn) {
	cli.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int64
		if err := watcher.QueryRow(cli.ctx(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'crash_child'`).Scan(&n); err != nil {
			cli.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			cli.t.Fatalf("%d sessions of the killed child remain", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const (
	cliHookKey   = "hook/batch-1/0/report"
	cliApplyKey  = "apply/batch-1/0/report"
	cliLandingAt = "merged-1"
)

type cliOutputDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type cliEffect struct {
	Key         string            `json:"key"`
	Member      string            `json:"member"`
	Op          string            `json:"op"`
	Kind        string            `json:"kind"`
	State       string            `json:"state"`
	Held        bool              `json:"held"`
	Attempts    int64             `json:"attempts"`
	LastAttempt json.RawMessage   `json:"last_attempt"`
	Result      json.RawMessage   `json:"result"`
	Outputs     []cliOutputDigest `json:"outputs"`
	At          string            `json:"at"`
}

// newEffectCLI is repository r with batch-1 landed and a hook intent for each key, as LandBatch leaves it. LandBatch is
// built separately; until it is, the test writes the effect rows directly.
func newEffectCLI(t *testing.T, keys ...string) batchCLI {
	t.Helper()
	cli := newBatchCLI(t)
	cli.landed("batch-1", keys...)
	return cli
}

// landed creates batch id, landed, with a hook intent carrying the landing commit for each key, as LandBatch records one
// for each post_land_effects entry.
func (cli batchCLI) landed(id string, keys ...string) {
	cli.t.Helper()
	cli.create(id, cliRecord("landed"))
	cli.setAdmission(id, "landed", "")
	for _, key := range keys {
		cli.sql(`INSERT INTO batch_effects (repo, batch, key, op, result) VALUES ('r', $1, $2, 'hook', jsonb_build_object('commit', $3::text))`,
			id, key, cliLandingAt)
	}
}

func (cli batchCLI) effects(batch string) []cliEffect {
	cli.t.Helper()
	var effects []cliEffect
	cli.decode(cli.invoke("effects", "r", batch), &effects)
	return effects
}

func (cli batchCLI) effect(batch, key string) cliEffect {
	cli.t.Helper()
	for _, effect := range cli.effects(batch) {
		if effect.Key == key {
			return effect
		}
	}
	cli.t.Fatalf("batch %s has no effect %s", batch, key)
	return cliEffect{}
}

// file writes contents to a new file and returns its path.
func (cli batchCLI) file(name string, contents []byte) string {
	cli.t.Helper()
	path := filepath.Join(cli.t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		cli.t.Fatal(err)
	}
	return path
}

func (cli batchCLI) jsonFile(value any) string {
	cli.t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		cli.t.Fatal(err)
	}
	return cli.file("value.json", body)
}

// items lists the work items of repository r as "id owner state", by id.
func (cli batchCLI) items() []string {
	cli.t.Helper()
	conn := cli.connect()
	defer conn.Close(cli.ctx())
	rows, err := conn.Query(cli.ctx(), `SELECT id || ' ' || owner || ' ' || state FROM work_items WHERE repo = 'r' ORDER BY id`)
	if err != nil {
		cli.t.Fatal(err)
	}
	defer rows.Close()
	items := []string{}
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			cli.t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

// state is what an effect command can change, as the command line shows it.
type cliState struct {
	Effects   string
	Contracts string
	Events    string
	Items     []string
}

func (cli batchCLI) state(batches ...string) cliState {
	cli.t.Helper()
	var effects strings.Builder
	for _, batch := range batches {
		effects.WriteString(cli.invoke("effects", "r", batch))
	}
	return cliState{
		Effects:   effects.String(),
		Contracts: cli.invoke("contract", "list", "r"),
		Events:    cli.invoke("--json", "events", "r", "0"),
		Items:     cli.items(),
	}
}

func (cli batchCLI) assertState(before cliState, batches ...string) {
	cli.t.Helper()
	if after := cli.state(batches...); !reflect.DeepEqual(after, before) {
		cli.t.Fatalf("the ledger changed:\n got %+v\nwant %+v", after, before)
	}
}

// refused runs args and requires exit 1 with the refusal reason on stderr.
func (cli batchCLI) refused(reason string, args ...string) {
	cli.t.Helper()
	code, stdout, stderr := cli.call(args...)
	if code != 1 || !strings.Contains(stderr, "refused ("+reason+")") {
		cli.t.Fatalf("%v: code=%d stdout=%q stderr=%q, want refusal %s", args, code, stdout, stderr, reason)
	}
}

func sameJSONText(t *testing.T, raw json.RawMessage, want any) bool {
	t.Helper()
	var got, expected any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
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

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// effects lists a batch's effects; effect-attempt records failed runs, leaving the effect an intent with its count and
// last result; effect-done completes it. A done repeated with the same result prints the stored row and writes
// nothing; a done with another result and an attempt on a done row exit 1 with effect_done; an unknown key exits 3.
// Each prints JSON.
func TestEffectCommandsAttemptThenDone(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey)
	intent := cli.effect("batch-1", cliHookKey)
	if intent.Op != "hook" || intent.State != "intent" || intent.Attempts != 0 || string(intent.LastAttempt) != "null" ||
		!sameJSONText(t, intent.Result, map[string]any{"commit": cliLandingAt}) || intent.Outputs == nil || len(intent.Outputs) != 0 {
		t.Fatalf("effects shows the intent as %+v", intent)
	}
	failure := map[string]any{"outcome": "timeout"}
	var attempted cliEffect
	cli.decode(cli.invoke("effect-attempt", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(failure),
		"--escalate-to", cliEscalationOwner, "--escalate-after", "10"), &attempted)
	if attempted.State != "intent" || attempted.Attempts != 1 || !sameJSONText(t, attempted.LastAttempt, failure) {
		t.Fatalf("effect-attempt printed %+v", attempted)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active after an attempt = %q", active)
	}

	result := cli.jsonFile(map[string]any{"outcome": "exit", "exit": 0})
	var done cliEffect
	cli.decode(cli.invoke("effect-done", "r", "batch-1", cliHookKey, "--result-file", result), &done)
	if done.State != "done" || done.Attempts != 1 || !sameJSONText(t, done.Result, map[string]any{"outcome": "exit", "exit": 0}) {
		t.Fatalf("effect-done printed %+v", done)
	}
	if active := cli.active(); len(active) != 0 {
		t.Fatalf("batches --active after the done = %q", active)
	}
	before := cli.state("batch-1")
	var replayed cliEffect
	cli.decode(cli.invoke("effect-done", "r", "batch-1", cliHookKey, "--result-file", result), &replayed)
	if !reflect.DeepEqual(replayed, done) {
		t.Fatalf("the repeat printed %+v, want %+v", replayed, done)
	}
	cli.refused("effect_done", "effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"outcome": "exit", "exit": 1}))
	cli.refused("effect_done", "effect-attempt", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(failure),
		"--escalate-to", cliEscalationOwner, "--escalate-after", "1")
	for _, args := range [][]string{
		{"effect-attempt", "r", "batch-1", "hook/batch-1/9/missing", "--result-file", cli.jsonFile(failure), "--escalate-to", cliEscalationOwner, "--escalate-after", "1"},
		{"effect-done", "r", "batch-1", "hook/batch-1/9/missing", "--result-file", result},
		{"effects", "r", "batch-9"},
		{"effects", "missing", "batch-1"},
	} {
		if code, stdout, stderr := cli.call(args...); code != 3 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 3", args, code, stdout, stderr)
		}
	}
	cli.assertState(before, "batch-1")
}

// effect-done with --then-file and two --output files stores the outputs, which effects shows with their SHA-256 and
// effect-outputs writes back byte for byte, and creates the follow-ons, each with the hook's commit, its key and its
// input. A repeat changes nothing.
func TestEffectDoneCommandStoresOutputsAndFollowOns(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey)
	ops := []byte(`{"op":"register","name":"C","owner":"alice","statement_hash":"h1","version_read":0}` + "\n")
	binary := []byte{0, 1, 2, 0xff, '\n', '='}
	args := []string{"effect-done", "r", "batch-1", cliHookKey,
		"--result-file", cli.jsonFile(map[string]any{"outcome": "exit", "exit": 0}),
		"--then-file", cli.jsonFile([]map[string]any{
			{"key": cliApplyKey, "op": "apply", "kind": "contracts", "input": "ops.jsonl"},
			{"key": "publish/batch-1/0/report", "op": "publish"},
		}),
		"--output", "ops.jsonl=" + cli.file("ops.jsonl", ops),
		"--output", "report.bin=" + cli.file("a=b.bin", binary),
	}
	var done cliEffect
	cli.decode(cli.invoke(args...), &done)
	want := []cliOutputDigest{{"ops.jsonl", digest(ops)}, {"report.bin", digest(binary)}}
	if done.State != "done" || !reflect.DeepEqual(done.Outputs, want) || !reflect.DeepEqual(cli.effect("batch-1", cliHookKey).Outputs, want) {
		t.Fatalf("effect-done printed %+v, want the outputs %+v", done, want)
	}
	for _, follow := range []struct{ key, op, kind, input string }{
		{cliApplyKey, "apply", "contracts", "ops.jsonl"},
		{"publish/batch-1/0/report", "publish", "", ""},
	} {
		effect := cli.effect("batch-1", follow.key)
		if effect.Op != follow.op || effect.Kind != follow.kind || effect.State != "intent" ||
			!sameJSONText(t, effect.Result, map[string]any{"commit": cliLandingAt, "hook": cliHookKey, "input": follow.input}) {
			t.Fatalf("the follow-on %s is %+v", follow.key, effect)
		}
	}
	dir := t.TempDir()
	var written []cliOutputDigest
	cli.decode(cli.invoke("effect-outputs", "r", "batch-1", cliHookKey, "--dir", dir), &written)
	if !reflect.DeepEqual(written, want) {
		t.Fatalf("effect-outputs printed %+v, want %+v", written, want)
	}
	for name, contents := range map[string][]byte{"ops.jsonl": ops, "report.bin": binary} {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(contents) {
			t.Fatalf("%s holds %q (%v), want %q", name, got, err, contents)
		}
		if info, err := os.Lstat(path); err != nil || info.Mode() != 0o600 {
			t.Fatalf("%s has mode %v (%v), want a regular file with mode 0600", name, info.Mode(), err)
		}
	}
	before := cli.state("batch-1")
	cli.invoke(args...)
	cli.assertState(before, "batch-1")
}

// effect-outputs writes nothing and exits 2 into a missing directory, a file, a non-empty directory, a directory that
// holds a symlink named like an output, or when a stored name is not a safe file name.
func TestEffectOutputsCommandWritesOnlyIntoAnEmptyDirectory(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey, "hook/batch-1/1/score")
	cli.invoke("effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
		"--output", "report.txt="+cli.file("report.txt", []byte("report\n")))
	for _, name := range []string{"../x", ".hidden", "a/b", "-dash", strings.Repeat("n", 65)} {
		t.Run("a stored name "+strconv.Quote(name), func(t *testing.T) {
			key := "hook/batch-1/" + strconv.Itoa(len(name)) + "/" + strings.ReplaceAll(name, "/", "_")
			cli.sql(`INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'batch-1', $1, 'hook')`, key)
			cli.invoke("effect-done", "r", "batch-1", key, "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
				"--output", name+"="+cli.file("payload", []byte("payload\n")))
			parent := t.TempDir()
			dir := filepath.Join(parent, "out")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := cli.call("effect-outputs", "r", "batch-1", key, "--dir", dir)
			if code != 2 || !strings.Contains(stderr, "not a safe output name") {
				t.Fatalf("effect-outputs of %q: code=%d stdout=%q stderr=%q, want exit 2", name, code, stdout, stderr)
			}
			for _, d := range []string{dir, parent} {
				if entries, err := os.ReadDir(d); err != nil || len(entries) != map[string]int{dir: 0, parent: 1}[d] {
					t.Fatalf("effect-outputs of %q wrote into %s: %v %v", name, d, entries, err)
				}
			}
		})
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withSymlink := t.TempDir()
	if err := os.Symlink(target, filepath.Join(withSymlink, "report.txt")); err != nil {
		t.Fatal(err)
	}
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "other"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, dir, stderr string }{
		{"a missing directory", filepath.Join(t.TempDir(), "missing"), "no such file"},
		{"a file", target, "not a directory"},
		{"a non-empty directory", nonEmpty, "is not empty"},
		{"a directory with a symlink named like an output", withSymlink, "is not empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, _ := os.ReadDir(test.dir)
			code, stdout, stderr := cli.call("effect-outputs", "r", "batch-1", cliHookKey, "--dir", test.dir)
			if code != 2 || !strings.Contains(stderr, test.stderr) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want exit 2 naming %q", code, stdout, stderr, test.stderr)
			}
			if after, _ := os.ReadDir(test.dir); !reflect.DeepEqual(after, before) {
				t.Fatalf("the directory changed from %v to %v", before, after)
			}
		})
	}
	if kept, err := os.ReadFile(target); err != nil || string(kept) != "keep\n" {
		t.Fatalf("the symlink's target holds %q (%v)", kept, err)
	}
	if code, _, stderr := cli.call("effect-outputs", "r", "batch-1", "hook/batch-1/1/score", "--dir", t.TempDir()); code != 0 {
		t.Fatalf("effect-outputs of an intent with no outputs: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := cli.call("effect-outputs", "r", "batch-1", "hook/batch-1/9/missing", "--dir", t.TempDir()); code != 3 {
		t.Fatalf("effect-outputs of an unknown effect: code=%d stderr=%q, want exit 3", code, stderr)
	}
}

// effect-attempt up to --escalate-after enqueues landing:<batch>:<key>@<n> once; a deterministic attempt holds the
// effect and escalates at once, and batches --active still lists the batch; effect-release clears the hold with an
// event that names the reason, and a second deterministic attempt gets its own item.
func TestEffectAttemptCommandEscalatesAndHolds(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey, "hook/batch-1/1/score")
	attempt := func(key string, result map[string]any) cliEffect {
		var effect cliEffect
		cli.decode(cli.invoke("effect-attempt", "r", "batch-1", key, "--result-file", cli.jsonFile(result),
			"--escalate-to", cliEscalationOwner, "--escalate-after", "2"), &effect)
		return effect
	}
	threshold := "landing:batch-1:" + cliHookKey + "@2 " + cliEscalationOwner + " open"
	for n := 1; n <= 3; n++ {
		attempt(cliHookKey, map[string]any{"outcome": "exit", "exit": 75})
		want := []string{}
		if n >= 2 {
			want = []string{threshold}
		}
		if got := cli.items(); !slices.Equal(got, want) {
			t.Fatalf("after attempt %d the items are %q, want %q", n, got, want)
		}
	}

	held := attempt("hook/batch-1/1/score", map[string]any{"deterministic": true, "reason": "output_missing"})
	first := "landing:batch-1:hook/batch-1/1/score@1 " + cliEscalationOwner + " open"
	if !held.Held || held.State != "intent" || !slices.Equal(cli.items(), []string{threshold, first}) {
		t.Fatalf("a deterministic attempt left %+v and the items %q", held, cli.items())
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active with a held effect = %q", active)
	}
	var released cliEffect
	cli.decode(cli.invoke("effect-release", "r", "batch-1", "hook/batch-1/1/score", "--reason", "the output path is fixed"), &released)
	if released.Held || released.State != "intent" {
		t.Fatalf("effect-release printed %+v", released)
	}
	var events []struct {
		Kind  string          `json:"kind"`
		Login string          `json:"login"`
		Data  json.RawMessage `json:"data"`
	}
	cli.decode(cli.invoke("--json", "events", "r", "0", "batch-1"), &events)
	releases := 0
	for _, e := range events {
		if e.Kind == "effect.released" {
			releases++
			if e.Login == "" || !sameJSONText(t, e.Data, map[string]any{"key": "hook/batch-1/1/score", "reason": "the output path is fixed"}) {
				t.Fatalf("the release event is %+v", e)
			}
		}
	}
	if releases != 1 {
		t.Fatalf("%d release events, want 1", releases)
	}
	attempt("hook/batch-1/1/score", map[string]any{"deterministic": true, "reason": "output_missing"})
	second := "landing:batch-1:hook/batch-1/1/score@2 " + cliEscalationOwner + " open"
	if got := cli.items(); !slices.Equal(got, []string{threshold, first, second}) {
		t.Fatalf("after the second deterministic attempt the items are %q", got)
	}
}

// effect-done --escalate-to enqueues landing:<batch>:<key>, and its repeat enqueues nothing.
func TestEffectDoneCommandEscalates(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey)
	args := []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"outcome": "exit", "exit": 2}),
		"--escalate-to", cliEscalationOwner}
	cli.invoke(args...)
	want := []string{"landing:batch-1:" + cliHookKey + " " + cliEscalationOwner + " open"}
	if got := cli.items(); !slices.Equal(got, want) {
		t.Fatalf("items after effect-done --escalate-to = %q", got)
	}
	before := cli.state("batch-1")
	cli.invoke(args...)
	cli.assertState(before, "batch-1")
}

// effect-apply-contracts applies the hook's stored operations: a new contract, an unchanged one and two librarian
// lines. contract list shows the new one, events its registration with the landing commit and the effect key, and
// effects the apply done and the librarian intents. A repeat exits 1 with effect_done; a conflict exits 1 with
// contract_conflict, and contract list, events and effects are unchanged.
func TestEffectApplyContractsCommand(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey, "hook/batch-1/1/report")
	line := func(value map[string]any) string {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(body) + "\n"
	}
	librarian := func(id string) map[string]any {
		return map[string]any{"op": "librarian", "id": id, "kind": "near_pair", "proposer": "owner:alice", "envelope": "proposal " + id}
	}
	ops := line(map[string]any{"op": "register", "name": "New", "owner": "owner:alice", "statement_hash": "h-new", "version_read": 0}) +
		line(librarian("near-1")) +
		line(map[string]any{"op": "register", "name": "Same", "owner": "owner:bob", "statement_hash": "h-same", "version_read": 0}) +
		line(librarian("near-2"))
	cli.invoke("contract", "register", "r", "Same", "owner:bob", "h-same", "false")
	cli.invoke("effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
		"--then-file", cli.jsonFile([]map[string]any{{"key": cliApplyKey, "op": "apply", "kind": "contracts", "input": "ops.jsonl"}}),
		"--output", "ops.jsonl="+cli.file("ops.jsonl", []byte(ops)))

	var applied cliEffect
	cli.decode(cli.invoke("effect-apply-contracts", "r", "batch-1", cliApplyKey), &applied)
	if applied.State != "done" || !sameJSONText(t, applied.Result, map[string]any{"applied": []string{"New"}, "unchanged": []string{"Same"}}) {
		t.Fatalf("effect-apply-contracts printed %+v", applied)
	}
	var contracts []struct{ Name, Owner string }
	cli.decode(cli.invoke("contract", "list", "r"), &contracts)
	if len(contracts) != 2 || contracts[0].Name != "New" || contracts[0].Owner != "owner:alice" {
		t.Fatalf("contract list = %+v", contracts)
	}
	var events []struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	cli.decode(cli.invoke("--json", "events", "r", "0", "New"), &events)
	if len(events) != 1 || events[0].Kind != "contract.registered" || !sameJSONText(t, events[0].Data, map[string]any{
		"statement_hash": "h-new", "version": 1, "pinned": false, "batch": "batch-1", "effect": cliApplyKey, "commit": cliLandingAt,
	}) {
		t.Fatalf("events of New = %+v", events)
	}
	for _, id := range []string{"near-1", "near-2"} {
		if intent := cli.effect("batch-1", "librarian/batch-1/"+id); intent.Op != "librarian" || intent.State != "intent" ||
			!sameJSONText(t, intent.Result, map[string]any{"line": librarian(id), "commit": cliLandingAt, "hook": cliHookKey}) {
			t.Fatalf("the librarian intent %s is %+v", id, intent)
		}
	}
	before := cli.state("batch-1")
	cli.refused("effect_done", "effect-apply-contracts", "r", "batch-1", cliApplyKey)
	cli.assertState(before, "batch-1")

	// A second hook's apply registers New for another owner, as a landing that read the contracts before this one did.
	cli.invoke("effect-done", "r", "batch-1", "hook/batch-1/1/report", "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
		"--then-file", cli.jsonFile([]map[string]any{{"key": "apply/batch-1/1/report", "op": "apply", "kind": "contracts", "input": "ops.jsonl"}}),
		"--output", "ops.jsonl="+cli.file("ops.jsonl", []byte(line(librarian("near-3"))+
			line(map[string]any{"op": "register", "name": "New", "owner": "owner:carol", "statement_hash": "h-new", "version_read": 0}))))
	before = cli.state("batch-1")
	code, stdout, stderr := cli.call("effect-apply-contracts", "r", "batch-1", "apply/batch-1/1/report")
	if code != 1 || !strings.Contains(stderr, "refused (contract_conflict)") || !strings.Contains(stderr, "line 2") {
		t.Fatalf("the conflicting apply: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	cli.assertState(before, "batch-1")
	if active := cli.active(); !slices.Equal(active, []string{"batch-1"}) {
		t.Fatalf("batches --active while effects are intents = %q", active)
	}
}

// Two batches with the same effect key each record, attempt and complete their own effect.
func TestEffectCommandsScopeKeysToTheirBatch(t *testing.T) {
	cli := newEffectCLI(t, "score")
	cli.landed("batch-2", "score")
	second := cli.effect("batch-2", "score")
	cli.invoke("effect-attempt", "r", "batch-1", "score", "--result-file", cli.jsonFile(map[string]any{"exit": 75}),
		"--escalate-to", cliEscalationOwner, "--escalate-after", "5")
	cli.invoke("effect-done", "r", "batch-1", "score", "--result-file", cli.jsonFile(map[string]any{"outcome": "pushed"}))
	if got := cli.effect("batch-2", "score"); !reflect.DeepEqual(got, second) {
		t.Fatalf("batch-1's calls changed batch-2's score: %+v", got)
	}
	if active := cli.active(); !slices.Equal(active, []string{"batch-2"}) {
		t.Fatalf("batches --active = %q", active)
	}
}

// begin-withdraw records withdraw/<id>, and a repeat prints the stored row. effect-skip completes an intent with the
// login and the reason in its result and its event, and creates no follow-on; the batch leaves batches --active once
// its other effect is done.
func TestWithdrawAndSkipCommands(t *testing.T) {
	cli := newBatchCLI(t)
	cli.create("publishing", cliRecord("publishing"))
	cli.setAdmission("publishing", "admitted", "")
	var withdraw cliEffect
	cli.decode(cli.invoke("begin-withdraw", "r", "publishing"), &withdraw)
	if withdraw.Key != "withdraw/publishing" || withdraw.Op != "withdraw" || withdraw.State != "intent" {
		t.Fatalf("begin-withdraw printed %+v", withdraw)
	}
	before := cli.state("publishing")
	var repeated cliEffect
	cli.decode(cli.invoke("begin-withdraw", "r", "publishing"), &repeated)
	if !reflect.DeepEqual(repeated, withdraw) {
		t.Fatalf("the repeat printed %+v, want %+v", repeated, withdraw)
	}
	cli.assertState(before, "publishing")

	cli.landed("batch-1", cliHookKey, "hook/batch-1/1/score")
	var skipped cliEffect
	cli.decode(cli.invoke("effect-skip", "r", "batch-1", cliHookKey, "--reason", "its input is gone"), &skipped)
	var login string
	if !sameJSONText(t, skipped.Result, map[string]any{"skipped_by": sessionLogin(t, cli.dsn), "reason": "its input is gone"}) || skipped.State != "done" {
		t.Fatalf("effect-skip printed %+v", skipped)
	}
	var events []struct {
		Kind  string          `json:"kind"`
		Login string          `json:"login"`
		Data  json.RawMessage `json:"data"`
	}
	cli.decode(cli.invoke("--json", "events", "r", "0", "batch-1"), &events)
	for _, e := range events {
		if e.Kind == "effect.skipped" {
			login = e.Login
			if !sameJSONText(t, e.Data, map[string]any{"key": cliHookKey, "reason": "its input is gone"}) {
				t.Fatalf("the skip event is %+v", e)
			}
		}
	}
	if login != sessionLogin(t, cli.dsn) {
		t.Fatalf("the skip event names login %q, want %q", login, sessionLogin(t, cli.dsn))
	}
	if keys := len(cli.effects("batch-1")); keys != 2 {
		t.Fatalf("the skip created a follow-on: %d effects", keys)
	}
	if active := cli.active(); !slices.Equal(active, []string{"publishing", "batch-1"}) {
		t.Fatalf("batches --active after the skip = %q", active)
	}
	cli.invoke("effect-done", "r", "batch-1", "hook/batch-1/1/score", "--result-file", cli.jsonFile(map[string]any{"exit": 0}))
	if active := cli.active(); !slices.Equal(active, []string{"publishing"}) {
		t.Fatalf("batches --active after the last done = %q", active)
	}
}

func sessionLogin(t *testing.T, dsn string) string {
	t.Helper()
	user, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Username()
}

// A malformed effect command exits 2 and changes nothing.
func TestEffectCommandsRefuseMalformedInput(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey)
	before := cli.state("batch-1")
	result := cli.jsonFile(map[string]any{"exit": 0})
	for _, test := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"effects with too few arguments", []string{"effects", "r"}, "effects <repo> <batch>"},
		{"begin-withdraw with too many arguments", []string{"begin-withdraw", "r", "batch-1", "x"}, "begin-withdraw <repo> <batch>"},
		{"effect-attempt without --escalate-after", []string{"effect-attempt", "r", "batch-1", cliHookKey, "--result-file", result, "--escalate-to", "o"}, "effect-attempt <repo>"},
		{"effect-attempt with a negative threshold", []string{"effect-attempt", "r", "batch-1", cliHookKey, "--result-file", result, "--escalate-to", "o", "--escalate-after", "-1"}, "escalate-after"},
		{"effect-attempt with an unknown flag", []string{"effect-attempt", "r", "batch-1", cliHookKey, "--result-file", result, "--escalate-to", "o", "--escalate-after", "1", "--x", "y"}, "unknown"},
		{"effect-done without --result-file", []string{"effect-done", "r", "batch-1", cliHookKey}, "effect-done <repo>"},
		{"effect-done with a result that is not JSON", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.file("bad.json", []byte("{"))}, "invalid JSON"},
		{"effect-done with two JSON values", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.file("two.json", []byte("{} {}"))}, "one JSON value"},
		{"effect-done with a missing result file", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", filepath.Join(t.TempDir(), "missing")}, "no such file"},
		{"effect-done with an unknown follow-on field", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", result, "--then-file", cli.jsonFile([]map[string]any{{"key": "k", "op": "apply", "topic": "t"}})}, "unknown field"},
		{"effect-done with an output without a name", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", result, "--output", "=" + result}, "--output <name>=<file>"},
		{"effect-done with an output without a file", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", result, "--output", "ops.jsonl"}, "--output <name>=<file>"},
		{"effect-done with a missing output file", []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", result, "--output", "ops.jsonl=" + filepath.Join(t.TempDir(), "missing")}, "no such file"},
		{"effect-outputs without --dir", []string{"effect-outputs", "r", "batch-1", cliHookKey}, "effect-outputs <repo>"},
		{"effect-apply-contracts with a flag", []string{"effect-apply-contracts", "r", "batch-1", cliHookKey, "--commit", "c"}, "effect-apply-contracts <repo>"},
		{"effect-skip without --reason", []string{"effect-skip", "r", "batch-1", cliHookKey}, "effect-skip <repo>"},
		{"effect-release without --reason", []string{"effect-release", "r", "batch-1", cliHookKey, "--why", "x"}, "effect-release <repo>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := cli.call(test.args...)
			if code != 2 || !strings.Contains(stderr, test.stderr) {
				t.Fatalf("%v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout, stderr, test.stderr)
			}
		})
	}
	cli.assertState(before, "batch-1")
}

// effectCommandChildEnv names the database and the command line of TestEffectCommandCrashChild, one per line (an
// environment value cannot hold a NUL byte, and no argument here holds a newline).
// It is set only in the child process that the effect command crash tests start.
const effectCommandChildEnv = "LEDGER_TEST_EFFECT_COMMAND_CHILD"

// TestEffectCommandCrashChild is not a test of its own: it runs one ledger command in a child process, then kills
// that process with SIGKILL right after the command returns, before any cleanup.
func TestEffectCommandCrashChild(t *testing.T) {
	fields := strings.Split(os.Getenv(effectCommandChildEnv), "\n")
	if len(fields) < 2 {
		t.Skip("run only as the child process of the effect command crash tests")
	}
	if code, _, _ := runLedger(fields[0], fields[1:]...); code != 0 {
		os.Exit(code)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

// runKilledAfterCommit runs args in a child process that is killed right after the command returns.
func (cli batchCLI) runKilledAfterCommit(args ...string) {
	cli.t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestEffectCommandCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), effectCommandChildEnv+"="+strings.Join(append([]string{cli.dsn}, args...), "\n"))
	err := child.Run()
	if child.ProcessState == nil {
		cli.t.Fatalf("the child did not run: %v", err)
	}
	status, ok := child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		cli.t.Fatalf("the child ended with %v (%v), want SIGKILL", child.ProcessState, err)
	}
}

// A ledger process killed right after an effect command commits leaves every write of the command, which a new ledger
// process reads back: a done with its follow-ons and outputs, the attempt that reaches the threshold with exactly one
// escalation item, a done with its escalation item, and a contracts apply with its registrations and librarian
// intents.
func TestEffectCommandsSurviveAKillAfterTheCommit(t *testing.T) {
	t.Run("effect-done --then-file --output", func(t *testing.T) {
		cli := newEffectCLI(t, cliHookKey)
		ops := []byte("{\"op\":\"librarian\",\"id\":\"near-1\",\"kind\":\"near_pair\",\"proposer\":\"p\",\"envelope\":\"e\"}\n")
		args := []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
			"--then-file", cli.jsonFile([]map[string]any{{"key": cliApplyKey, "op": "apply", "kind": "contracts", "input": "ops.jsonl"}}),
			"--output", "ops.jsonl=" + cli.file("ops.jsonl", ops), "--output", "log.txt=" + cli.file("log.txt", []byte("log\n"))}
		cli.runKilledAfterCommit(args...)
		if done := cli.effect("batch-1", cliHookKey); done.State != "done" ||
			!reflect.DeepEqual(done.Outputs, []cliOutputDigest{{"log.txt", digest([]byte("log\n"))}, {"ops.jsonl", digest(ops)}}) {
			t.Fatalf("after the kill the hook is %+v", done)
		}
		if apply := cli.effect("batch-1", cliApplyKey); apply.State != "intent" {
			t.Fatalf("after the kill the follow-on is %+v", apply)
		}
		before := cli.state("batch-1")
		cli.invoke(args...)
		cli.assertState(before, "batch-1")
		cli.runKilledAfterCommit("effect-apply-contracts", "r", "batch-1", cliApplyKey)
		if apply, librarian := cli.effect("batch-1", cliApplyKey), cli.effect("batch-1", "librarian/batch-1/near-1"); apply.State != "done" || librarian.State != "intent" {
			t.Fatalf("after a kill after effect-apply-contracts: apply %+v, librarian %+v", apply, librarian)
		}
	})
	t.Run("effect-attempt at the threshold", func(t *testing.T) {
		cli := newEffectCLI(t, cliHookKey)
		args := []string{"effect-attempt", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 75}),
			"--escalate-to", cliEscalationOwner, "--escalate-after", "2"}
		cli.invoke(args...)
		cli.runKilledAfterCommit(args...)
		if got := cli.items(); !slices.Equal(got, []string{"landing:batch-1:" + cliHookKey + "@2 " + cliEscalationOwner + " open"}) {
			t.Fatalf("after the kill the items are %q", got)
		}
		if effect := cli.effect("batch-1", cliHookKey); effect.Attempts != 2 {
			t.Fatalf("after the kill the effect is %+v", effect)
		}
	})
	t.Run("effect-done --escalate-to", func(t *testing.T) {
		cli := newEffectCLI(t, cliHookKey)
		args := []string{"effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 3}),
			"--escalate-to", cliEscalationOwner}
		cli.runKilledAfterCommit(args...)
		if got := cli.items(); !slices.Equal(got, []string{"landing:batch-1:" + cliHookKey + " " + cliEscalationOwner + " open"}) ||
			cli.effect("batch-1", cliHookKey).State != "done" {
			t.Fatalf("after the kill the items are %q and the effect %+v", got, cli.effect("batch-1", cliHookKey))
		}
		before := cli.state("batch-1")
		cli.invoke(args...)
		cli.assertState(before, "batch-1")
	})
}

// A ledger process killed inside effect-done --then-file --output, after it marked the hook done and stored the
// outputs and while it waits to create the follow-on, leaves none of them. The test holds an uncommitted effect row
// with the follow-on's key, so the command waits on it; then it kills the command and rolls the row back.
func TestEffectDoneCommandKilledInsideTheTransactionLeavesNothing(t *testing.T) {
	cli := newEffectCLI(t, cliHookKey)
	before := cli.state("batch-1")
	conn := cli.connect()
	defer conn.Close(cli.ctx())
	holder, err := conn.Begin(cli.ctx())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(cli.ctx(), `INSERT INTO batch_effects (repo, batch, key, op) VALUES ('r', 'batch-1', $1, 'hold')`, cliApplyKey); err != nil {
		t.Fatal(err)
	}
	childDSN, err := url.Parse(cli.dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := childDSN.Query()
	query.Set("application_name", "crash_child")
	childDSN.RawQuery = query.Encode()
	args := []string{childDSN.String(), "effect-done", "r", "batch-1", cliHookKey, "--result-file", cli.jsonFile(map[string]any{"exit": 0}),
		"--then-file", cli.jsonFile([]map[string]any{{"key": cliApplyKey, "op": "apply", "kind": "contracts", "input": "ops.jsonl"}}),
		"--output", "ops.jsonl=" + cli.file("ops.jsonl", []byte("{}\n"))}
	child := exec.Command(os.Args[0], "-test.run=^TestEffectCommandCrashChild$", "-test.count=1")
	child.Env = append(os.Environ(), effectCommandChildEnv+"="+strings.Join(args, "\n"))
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	watcher := cli.connect()
	defer watcher.Close(cli.ctx())
	cli.waitForChildLock(watcher, "batch_effect_outputs")
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("the child was not killed")
	}
	if err := holder.Rollback(cli.ctx()); err != nil {
		t.Fatal(err)
	}
	cli.waitForNoChild(watcher)
	cli.assertState(before, "batch-1")
	if outputs := cli.invoke("effect-outputs", "r", "batch-1", cliHookKey, "--dir", t.TempDir()); strings.TrimSpace(outputs) != "[]" {
		t.Fatalf("after the kill effect-outputs printed %s", outputs)
	}
	cli.invoke(args[1:]...)
	if apply := cli.effect("batch-1", cliApplyKey); apply.State != "intent" || apply.Op != "apply" {
		t.Fatalf("the done after the kill left the follow-on %+v", apply)
	}
}
