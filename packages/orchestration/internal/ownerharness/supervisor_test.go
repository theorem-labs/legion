package ownerharness

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
	"github.com/sjawhar/legion/orchestration/owner/messagetest"
)

const (
	graphOwner = "function:a@x/y.c"
	roleOwner  = "librarian"
)

type supervisorFixture struct {
	t       *testing.T
	ctx     context.Context
	store   *ledger.Store
	config  Config
	path    string
	socket  string
	dsnFile string
}

func newSupervisorFixture(t *testing.T) *supervisorFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the supervisor harness runs owners in a real tmux server")
	}
	ctx := context.Background()
	dsn := testpg.Database(t, "owner_harness")
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The omp stand-in records the prompt file it was given and stays alive, as an idle session does. It is not on
	// PATH: the supervisor runs the omp that LEGION_OMP_PATH names, as the daemon does.
	omp := filepath.Join(bin, "omp")
	if err := os.WriteFile(omp, []byte("#!/bin/sh\necho \"$@\" > argv\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dsnFile := filepath.Join(dir, "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	operatorToken := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(operatorToken, []byte("operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "owner.md")
	if err := os.WriteFile(prompt, []byte("prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := "harness-" + strings.ToLower(time.Now().Format("150405.000000"))
	config := "project: " + project + "\nstate_dir: " + filepath.Join(dir, "state") + "\nruntime: tmux\npostgres_dsn: postgres://unused\n" +
		"operator_token_file: " + operatorToken + "\n" +
		"port: " + "0" + "\nenvoy_url: http://127.0.0.1:9\nstanding_owners:\n  ledger_dsn_file: " + dsnFile + "\n  repository: r\n" +
		"  max_running: 2\n  budget_usd: 100\n  reconcile_seconds: 1\n  never_start: [escalations]\n" +
		"  owners:\n    - {name: librarian, prompt_file: " + prompt + ", harness: omp, max_cost_usd: 1}\n" +
		"  templates:\n    function: {prompt_file: " + prompt + ", harness: omp, max_cost_usd: 1}\n"
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path, func(name string) string { return map[string]string{"LEGION_OMP_PATH": omp}[name] })
	if err != nil {
		t.Fatal(err)
	}
	f := &supervisorFixture{t: t, ctx: ctx, store: store, config: loaded, path: path, socket: messagetest.DaemonSocket(project), dsnFile: dsnFile}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", f.socket, "kill-server").Run() })
	return f
}

// testLog is the supervisor's log, shown with the test's output.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// start runs the supervisor until the returned stop is called.
func (f *supervisorFixture) start() (stop func()) {
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- Supervise(ctx, f.config, testLog{f.t}) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			f.t.Errorf("Supervise: %v", err)
		}
	}
}

func (f *supervisorFixture) panes() map[string]string {
	out, _ := exec.Command("tmux", "-L", f.socket, "list-panes", "-a", "-F", "#{pane_id} #{@legion_owner} #{pane_pid}").Output()
	panes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 {
			panes[fields[1]] = fields[0] + " " + fields[2]
		}
	}
	return panes
}

func (f *supervisorFixture) eventually(what string, ok func() bool) {
	f.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	f.t.Fatalf("timed out waiting for %s", what)
}

func environ(t *testing.T, pid string) map[string]string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, entry := range bytes.Split(data, []byte{0}) {
		if name, value, ok := strings.Cut(string(entry), "="); ok {
			env[name] = value
		}
	}
	return env
}

// An owner with open work starts from its kind's template in tmux, with the launch variables of C8 and the working
// directory named by its encoded claim token (C9); a named role starts from the owners list.
func TestSupervisorStartsAnOwnerFromItsTemplateWithTheLaunchVariables(t *testing.T) {
	f := newSupervisorFixture(t)
	stop := f.start()
	defer stop()
	for owner, item := range map[string]string{graphOwner: "start:" + graphOwner + "@1", roleOwner: "start:librarian@1"} {
		if err := f.store.Enqueue(f.ctx, "r", item, owner); err != nil {
			t.Fatal(err)
		}
	}
	f.eventually("both owner panes", func() bool { return len(f.panes()) == 2 })
	pid := strings.Fields(f.panes()[graphOwner])[1]
	env := environ(t, pid)
	for name, want := range map[string]string{"LEGION_OWNER": graphOwner, "LEGION_LEDGER_REPOSITORY": "r", "LEGION_LEDGER_DSN_FILE": f.dsnFile, "ENVOY_URL": "http://127.0.0.1:9"} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	dir, _ := os.Readlink("/proc/" + pid + "/cwd")
	if want := filepath.Join(f.config.StateDir, "owners", "legion-"+messagetest.ProjectToken(f.config.Project)+"-owner-"+messagetest.EncodeOwnerName(graphOwner)); dir != want {
		t.Fatalf("working directory = %q, want %q", dir, want)
	}
	if start, err := os.ReadFile(env["LEGION_OWNER_START_FILE"]); err != nil || !strings.Contains(string(start), graphOwner) {
		t.Fatalf("start file = %q, %v", start, err)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil || !strings.Contains(string(argv), f.config.StandingOwners.Templates["function"].PromptFile) {
		t.Fatalf("omp argv = %q, %v", argv, err)
	}
}

// An owner on never_start never starts, and an id of no template and no role gets one durable refusal.
func TestSupervisorNeverStartsListedOwnersAndRefusesUnknownOnes(t *testing.T) {
	f := newSupervisorFixture(t)
	stop := f.start()
	defer stop()
	for owner, item := range map[string]string{"escalations": "escalation:0123456789abcdef", "file:a.c": "start:file:a.c@1"} {
		if err := f.store.Enqueue(f.ctx, "r", item, owner); err != nil {
			t.Fatal(err)
		}
	}
	refusals := filepath.Join(f.config.StateDir, "refusals.jsonl")
	f.eventually("the refusal of file:a.c", func() bool {
		data, _ := os.ReadFile(refusals)
		return bytes.Contains(data, []byte(`"owner":"file:a.c"`))
	})
	time.Sleep(1500 * time.Millisecond) // a reconciliation sweep later
	if panes := f.panes(); len(panes) != 0 {
		t.Fatalf("panes = %v, want none", panes)
	}
	data, _ := os.ReadFile(refusals)
	if n := bytes.Count(data, []byte("\n")); n != 1 {
		t.Fatalf("refusals = %q, want one per work revision", data)
	}
}

// A dead owner pane frees its holder's leases and, while work remains, starts again; a restarted supervisor adopts
// the panes it finds instead of starting them twice.
func TestSupervisorReleasesAndRestartsADeadOwnerAndAdoptsAfterARestart(t *testing.T) {
	f := newSupervisorFixture(t)
	item := "start:" + graphOwner + "@1"
	if err := f.store.Enqueue(f.ctx, "r", item, graphOwner); err != nil {
		t.Fatal(err)
	}
	stop := f.start()
	f.eventually("the owner pane", func() bool { return f.panes()[graphOwner] != "" })
	if _, err := f.store.Claim(f.ctx, "r", item, graphOwner, 0); err != nil {
		t.Fatal(err)
	}
	// A pane id can come back when the killed pane was the server's last, so a relaunch is a new pane process.
	first := f.panes()[graphOwner]
	if err := exec.Command("tmux", "-L", f.socket, "kill-pane", "-t", strings.Fields(first)[0]).Run(); err != nil {
		t.Fatal(err)
	}
	f.eventually("a second pane", func() bool { p := f.panes()[graphOwner]; return p != "" && p != first })
	events, err := f.store.Events(f.ctx, "r", 0, item)
	if err != nil {
		t.Fatal(err)
	}
	stopped := 0
	for _, event := range events {
		if event.Kind == "item.holder_stopped" {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("holder_stopped events = %d, want 1: %+v", stopped, events)
	}
	second := f.panes()[graphOwner]
	stop()
	if got := f.panes()[graphOwner]; got != second {
		t.Fatalf("the pane after a supervisor stop = %q, want %q still running", got, second)
	}
	stop = f.start()
	defer stop()
	time.Sleep(1500 * time.Millisecond)
	if panes := f.panes(); len(panes) != 1 || panes[graphOwner] != second {
		t.Fatalf("panes after a restart = %v, want only the adopted %q", panes, second)
	}
}

func TestLoadConfigRefusesWhatTheMinimalFileDoesNotAllow(t *testing.T) {
	dir := t.TempDir()
	dsnFile := filepath.Join(dir, "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte("postgres://x"), 0o600); err != nil {
		t.Fatal(err)
	}
	operatorToken := filepath.Join(dir, "operator-token")
	if err := os.WriteFile(operatorToken, []byte("operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyToken := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(emptyToken, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "owner.md")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	omp := filepath.Join(dir, "omp")
	if err := os.WriteFile(omp, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := "project: p\nstate_dir: " + dir + "/state\noperator_token_file: " + operatorToken + "\nstanding_owners:\n  ledger_dsn_file: " + dsnFile +
		"\n  repository: r\n  max_running: 2\n  budget_usd: 10\n" +
		"  templates:\n    function: {prompt_file: " + prompt + ", harness: omp, max_cost_usd: 1}\n"
	path := filepath.Join(dir, "legion.yaml")
	load := func(text string, env map[string]string) (Config, error) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path, func(name string) string { return env[name] })
	}
	good := map[string]string{"LEGION_POSTGRES_DSN": "postgres://legion", "LEGION_OMP_PATH": omp}
	config, err := load(base, good)
	if err != nil {
		t.Fatalf("the minimal file with LEGION_POSTGRES_DSN and LEGION_OMP_PATH: %v", err)
	}
	if config.OMP != omp || config.OperatorTokenFile != operatorToken {
		t.Fatalf("config runs omp %q with operator token %q", config.OMP, config.OperatorTokenFile)
	}
	for name, text := range map[string]string{
		"no daemon database":           base,
		"no omp":                       base,
		"a relative LEGION_OMP_PATH":   base,
		"an omp_invocation only":       base + "omp_invocation: mise x omp -- omp\n",
		"no operator token file":       strings.Replace(base, "operator_token_file: "+operatorToken+"\n", "", 1),
		"an empty operator token file": strings.Replace(base, operatorToken, emptyToken, 1),
		"a missing operator token":     strings.Replace(base, operatorToken, filepath.Join(dir, "no-token"), 1),
		"an unknown key":               base + "dispatch_url: http://dispatch\n",
		"an unknown kind":              strings.Replace(base, "function:", "file:", 1),
		"a harness that is not omp":    strings.Replace(base, "harness: omp", "harness: claude", 1),
		"no owners or templates":       strings.Split(base, "  templates:")[0],
	} {
		env := map[string]string{"LEGION_POSTGRES_DSN": "postgres://legion", "LEGION_OMP_PATH": omp}
		switch name {
		case "no daemon database":
			delete(env, "LEGION_POSTGRES_DSN")
		case "no omp", "an omp_invocation only":
			delete(env, "LEGION_OMP_PATH")
		case "a relative LEGION_OMP_PATH":
			env["LEGION_OMP_PATH"] = "omp"
		}
		if _, err := load(text, env); err == nil {
			t.Errorf("LoadConfig accepted %s", name)
		}
	}
	if err := os.Chmod(dsnFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := load(base, good); err == nil {
		t.Error("LoadConfig accepted a ledger DSN file others can read")
	}
}
