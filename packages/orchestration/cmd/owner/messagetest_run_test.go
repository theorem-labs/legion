package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/orchestration/internal/ownerharness"
	"github.com/sjawhar/legion/orchestration/internal/testpg"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"github.com/sjawhar/legion/orchestration/owner/messagetest"
	"gopkg.in/yaml.v3"
)

const (
	mtFunction = "function:get_mountpoint@fs/namespace.c"
	mtStruct   = "struct:mountpoint"
	mtCaller   = "function:lock_mount@fs/namespace.c"
	mtCallee   = "function:kfree"
	mtLinked   = "struct:dentry"
)

// writeMessageTestGraph writes a deployment graph whose two test nodes hold every topic kind between them: the
// function has a caller and a callee and uses the struct, and the struct is linked to another.
func writeMessageTestGraph(t *testing.T, dir string) {
	t.Helper()
	node := func(id, suffix string) string {
		return "notifications.example.node." + ownerpkg.TopicToken(id) + "." + suffix
	}
	owners := []map[string]any{}
	for id, kind := range map[string]string{mtFunction: "function", mtStruct: "struct", mtCaller: "function", mtCallee: "function", mtLinked: "struct"} {
		role := map[string]string{"function": "function", "struct": "data_structure"}[kind]
		owners = append(owners, map[string]any{"id": id, "kind": kind, "role": role, "name": strings.SplitN(strings.SplitN(id, ":", 2)[1], "@", 2)[0]})
	}
	documents := map[string]any{
		"owners.json": map[string]any{"format": "cgraph/owners/v1", "part": "slice", "owners": owners},
		"topics.json": map[string]any{"format": "cgraph/topics/v1", "part": "slice", "prefix": "notifications.example.", "token_rule": "percent-encoding",
			"owners": map[string][]string{
				mtFunction: {"notifications.example.all", node(mtFunction, "needs"), node(mtCallee, "callers"), node(mtCallee, "neighbors"),
					node(mtCaller, "callees"), node(mtCaller, "neighbors"), node(mtStruct, "users"), node(mtStruct, "neighbors")},
				mtStruct: {"notifications.example.all", node(mtStruct, "needs"), node(mtFunction, "neighbors"), node(mtLinked, "linked"), node(mtLinked, "neighbors")},
			}},
		"graph.json": map[string]any{"format": "cgraph/graph/v1", "part": "slice", "nodes": []map[string]any{
			{"id": mtFunction, "kind": "function", "part": true}, {"id": mtCaller, "kind": "function", "part": true},
			{"id": mtCallee, "kind": "function", "part": true}, {"id": mtStruct, "kind": "struct"}, {"id": mtLinked, "kind": "struct"},
		}, "edges": []map[string]any{
			{"kind": "call", "source": mtCaller, "target": mtFunction}, {"kind": "call", "source": mtFunction, "target": mtCallee},
			{"kind": "access", "source": mtFunction, "target": mtStruct, "access": "read"}, {"kind": "embeds", "source": mtStruct, "target": mtLinked},
		}},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, document := range documents {
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// harnessRun is `owner message-test` against the harness: a ledger on real Postgres, the deployment graph of the
// test nodes, an HTTP stand-in listener, and the test binary linked as owner, legion and omp in bin. out is the run's
// directory and dsnFile the deployment's ledger DSN file.
type harnessRun struct {
	command      *exec.Cmd
	out, dsnFile string
	bin, state   string
}

func newHarnessRun(t *testing.T, ctx context.Context, env ...string) harnessRun {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the message test's owners run in a real tmux server")
	}
	// The run's tmux socket is under its directory and must fit a Unix socket's 107 bytes, which a directory named
	// for the test, as t.TempDir's is, would not leave room for.
	root, err := os.MkdirTemp("", "mt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dsnFile := filepath.Join(root, "ledger-dsn")
	if err := os.WriteFile(dsnFile, []byte(testpg.Database(t, "owner_message_test")), 0o600); err != nil {
		t.Fatal(err)
	}
	graphDir := filepath.Join(root, "graph")
	writeMessageTestGraph(t, graphDir)
	testdata, err := os.ReadFile(filepath.Join("..", "..", "owner", "testdata", "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deploy := filepath.Join(root, "deploy.yaml")
	stateDir := filepath.Join(root, "state")
	if err := os.WriteFile(deploy, []byte(strings.NewReplacer("/srv/owners/ledger-dsn", dsnFile, "/srv/graph/out", graphDir, "/srv/owners/state", stateDir).Replace(string(testdata))), 0o600); err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(ownerharness.NewListener())
	// A run that left its panes subscribed holds connections open, which Close would wait on.
	t.Cleanup(func() { listener.CloseClientConnections(); listener.Close() })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner", "legion", "omp"} {
		if err := os.Symlink(self, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(root, "run")
	command := exec.CommandContext(ctx, filepath.Join(bin, "owner"), "--deploy", deploy, "message-test",
		"--prefix", "notifications.example-message-test.", "--kills", "3", "--out", out)
	// The driver reads its ledger from the deployment's ledger_dsn_file alone: a launch variable in the operator's
	// shell names no ledger of the run.
	command.Env = append(append(os.Environ(), "PATH="+bin+string(filepath.ListSeparator)+os.Getenv("PATH"), "HOME="+root,
		"ENVOY_URL="+listener.URL, "ENVOY_TOKEN=message-test-token", "LEGION_POSTGRES_DSN=postgres://legion@127.0.0.1/legion",
		"OWNER_HARNESS_STEP_DELAY=300ms", "LEGION_LEDGER_DSN_FILE="+filepath.Join(root, "not-the-deployment-ledger")), env...)
	return harnessRun{command: command, out: out, dsnFile: dsnFile, bin: bin, state: stateDir}
}

// The message test runs end to end as `owner message-test`: real subprocesses, a real tmux server, the ledger on
// real Postgres and an HTTP listener. The supervisor and the sessions are internal/ownerharness's test-only
// stand-ins (linked as legion and omp), so this run checks the driver and the owner command, and is no evidence
// that the Legion daemon meets C8, C9 or C33.
func TestMessageTestRunsEndToEndAgainstTheSupervisorHarness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	// The owners take the out-of-order notices without recording them, as an owner model did in acceptance run 7:
	// the run judges deliveries by the sessions' transcripts.
	harness := newHarnessRun(t, ctx, "OWNER_HARNESS_SKIP_SAW=out of order")
	out, dsnFile, bin := harness.out, harness.dsnFile, harness.bin
	output, err := harness.command.CombinedOutput()
	reportText, _ := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatalf("owner message-test: %v\n%s\n%s", err, output, reportText)
	}
	var report messagetest.Report
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil || json.Unmarshal(data, &report) != nil {
		t.Fatalf("report.json: %v\n%s", err, output)
	}
	if !report.Passed || len(report.Failures) != 0 || !slices.Equal(report.Nodes, []string{mtFunction, mtStruct}) {
		t.Fatalf("report = %+v\n%s", report, reportText)
	}
	if len(report.Panes) != 2 {
		t.Fatalf("panes = %+v", report.Panes)
	}
	t.Logf("owner message-test:\n%s\n%s", output, reportText)
	for _, pane := range report.Panes {
		if pane.Env["LEGION_OWNER"] != pane.Owner || pane.Env["LEGION_LEDGER_REPOSITORY"] != report.Run || pane.Env["LEGION_LEDGER_DSN_FILE"] != dsnFile ||
			!strings.HasSuffix(pane.Dir, messagetest.EncodeOwnerName(pane.Owner)) || pane.StartSeconds > 60 {
			t.Fatalf("pane = %+v", pane)
		}
		// The driver published once the listener listed the session with every topic, under the run's own prefix.
		if len(pane.Sessions) == 0 || len(pane.Transcripts) != 1 || len(pane.Subscribed) == 0 || !sameTopics(pane.Subscribed, pane.Topics) ||
			!strings.HasPrefix(pane.Subscribed[0], "notifications.example-message-test."+report.Run+".") {
			t.Fatalf("pane %s: sessions %v, transcripts %v, the listener's topics %v, its subscribed record %v", pane.Owner, pane.Sessions, pane.Transcripts, pane.Subscribed, pane.Topics)
		}
	}
	kinds := map[string]bool{}
	for _, publication := range report.Published {
		if publication.Step == "arrival" {
			kinds[publication.Kind] = true
		}
	}
	for _, kind := range append(slices.Clone(messagetest.TopicKinds), "fact.stack-alignment.users", "fact.stack-alignment.needs", "concepts") {
		if !kinds[kind] {
			t.Errorf("no arrival notice of kind %s: %+v", kind, report.Published)
		}
	}
	if len(report.KillLog) != 3 || report.Restart == nil || report.Restart.ItemSeconds <= 0 {
		t.Fatalf("kills = %+v, restart = %+v", report.KillLog, report.Restart)
	}
	// Each kill landed at the moment the schedule set for it: before the claim, after the claim, and between the
	// answer's record and its completion, each time with the killed process gone before the hold was released.
	for index, kill := range report.KillLog {
		want := messagetest.Moments[index]
		if kill.Scheduled != want || kill.Observed != want || kill.Done || (want == messagetest.MomentAfterAnswer) != (kill.Record != "") ||
			(want == messagetest.MomentBeforeClaim) != (kill.Holder == "") || (want == messagetest.MomentBeforeClaim) != (kill.Held == 0) {
			t.Errorf("kill %d = %+v, want it to land %s", index+1, kill, want)
		}
	}
	// The new daemon did the restart's work: the old daemon's pane was killed, and every target pane that ran while
	// the item was open is one the new daemon logged as launched.
	restart := report.Restart
	if restart.NewDaemonPID == restart.OldDaemonPID || len(restart.WorkPanes) == 0 || restart.ClaimEvent <= restart.EventsFrom || restart.CompleteEvent <= restart.ClaimEvent {
		t.Fatalf("restart = %+v", restart)
	}
	for _, pane := range restart.WorkPanes {
		if !slices.Contains(restart.Launched, pane.Incarnation) || pane.Incarnation == restart.OldPane.Incarnation {
			t.Fatalf("the restart's item ran in %+v, which the new daemon did not launch (launched %v, old pane %+v)", pane, restart.Launched, restart.OldPane)
		}
	}
	for _, step := range []string{"setup", "start the daemon", "wake from the ledger", "arrival", "duplicates", "out of order", "kill 1 of 3 (before-claim)",
		"kill 2 of 3 (after-claim)", "kill 3 of 3 (after-answer)", "daemon restart", "collect", "teardown"} {
		if !slices.ContainsFunc(report.Steps, func(s messagetest.StepTiming) bool { return s.Name == step && s.Error == "" }) {
			t.Errorf("step %q did not run cleanly: %+v", step, report.Steps)
		}
	}
	assertTheRunsTmuxServerIsGone(t, out, report.Daemon.Socket)
	if !strings.Contains(string(output), "report "+filepath.Join(out, "report.md")) {
		t.Fatalf("the command did not print the report path:\n%s", output)
	}
	assertDuplicatesAndOrderRecorded(t, report)
	assertOwnerNoticesArrived(t, out, report)

	// The Envoy bearer came from ENVOY_TOKEN, so the supervisor read it from a private file outside the run's
	// directory, which the teardown removed.
	err = filepath.WalkDir(out, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "message-test-token") {
			t.Errorf("%s holds the Envoy token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The supervisor reads the Envoy bearer and its operator bearer from private files outside the run's directory,
	// which the teardown removed. It runs the omp that the driver names, and the report names that omp.
	var supervisor struct {
		EnvoyTokenFile    string `yaml:"envoy_token_file"`
		OperatorTokenFile string `yaml:"operator_token_file"`
	}
	data, err = os.ReadFile(filepath.Join(out, "daemon", "legion.yaml"))
	if err != nil || yaml.Unmarshal(data, &supervisor) != nil {
		t.Fatalf("legion.yaml: %v\n%s", err, data)
	}
	for key, file := range map[string]string{"envoy_token_file": supervisor.EnvoyTokenFile, "operator_token_file": supervisor.OperatorTokenFile} {
		if file == "" || strings.HasPrefix(file, out+string(filepath.Separator)) {
			t.Fatalf("legion.yaml %s = %q; want a file outside %s", key, file, out)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("the %s %s is left after the run: %v", key, file, err)
		}
	}
	var raw struct {
		Daemon map[string]any `json:"daemon"`
	}
	if data, err := os.ReadFile(filepath.Join(out, "report.json")); err != nil || json.Unmarshal(data, &raw) != nil || raw.Daemon["omp"] != filepath.Join(bin, "omp") {
		t.Fatalf("the report's daemon = %v (%v); want omp %s", raw.Daemon, err, filepath.Join(bin, "omp"))
	}

	// The report names what ran, and the stand-ins say they are stand-ins.
	revisions := report.Revisions
	if !strings.Contains(revisions.Legion, "test-only stand-in") || !strings.Contains(revisions.Omp, "test-only stand-in") ||
		revisions.Owner.GoVersion == "" || len(revisions.Graph) != 3 || !strings.Contains(string(reportText), "## Revisions") {
		t.Fatalf("revisions = %+v", revisions)
	}
}

// SIGINT to a running `owner message-test` stops the run: it stops its own daemon by its PID, its panes by its own tmux
// socket and every owner process it saw by its PID, removes its bearer files, and writes a partial report marked
// interrupted (acceptance run 7: cancelling the driver left the daemon, both panes and the bearer directory behind,
// and no report).
func TestMessageTestStopsWhatItStartedWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	harness := newHarnessRun(t, ctx)
	progress, err := harness.command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	harness.command.Stdout = &stdout
	if err := harness.command.Start(); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	lines := bufio.NewScanner(progress)
	for lines.Scan() {
		log.WriteString(lines.Text() + "\n")
		if strings.HasSuffix(lines.Text(), " arrival") {
			if err := harness.command.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	for lines.Scan() {
		log.WriteString(lines.Text() + "\n")
	}
	err = harness.command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("owner message-test after SIGINT: %v, want exit 1\n%s", err, log.String())
	}
	var report messagetest.Report
	data, err := os.ReadFile(filepath.Join(harness.out, "report.json"))
	if err != nil || json.Unmarshal(data, &report) != nil {
		t.Fatalf("the partial report.json: %v\n%s", err, log.String())
	}
	markdown, _ := os.ReadFile(filepath.Join(harness.out, "report.md"))
	if report.Passed || !strings.Contains(report.Interrupted, "interrupt") || !strings.Contains(string(markdown), "INTERRUPTED") {
		t.Fatalf("the report after SIGINT: passed %t, interrupted %q\n%s", report.Passed, report.Interrupted, markdown)
	}
	teardown := report.Teardown
	if len(teardown.DaemonPIDs) == 0 || len(teardown.OwnerPIDs) == 0 || len(teardown.Left) != 0 {
		t.Fatalf("the teardown = %+v", teardown)
	}
	for _, pid := range append(slices.Clone(teardown.DaemonPIDs), teardown.OwnerPIDs...) {
		if environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil && strings.Contains(string(environ), report.Run) {
			t.Fatalf("the run's process %d still runs", pid)
		}
	}
	assertTheRunsTmuxServerIsGone(t, harness.out, report.Daemon.Socket)
	if _, err := os.Stat(filepath.Join(harness.state, "message-test", report.Run)); !os.IsNotExist(err) || teardown.TokenDir == "" {
		t.Fatalf("the bearer directory %q after SIGINT: %v", teardown.TokenDir, err)
	}
	t.Logf("%s\n%s", log.String(), markdown)
}

// A notice delivered twice under two keys reaches the session twice and is answered once; the out-of-order notices
// and the restart's notice to its running owner are required deliveries, each in the session's transcript, and the
// out-of-order ones are in no record of the owner's (OWNER_HARNESS_SKIP_SAW), which decides nothing.
func assertDuplicatesAndOrderRecorded(t *testing.T, report messagetest.Report) {
	t.Helper()
	arrived, saw := map[string]bool{}, map[string]bool{}
	for _, delivery := range report.Deliveries {
		if delivery.Owner == mtStruct {
			arrived[delivery.ID], saw[delivery.ID] = delivery.Arrived, delivery.Saw
		}
	}
	required := map[string]bool{}
	for _, expected := range report.Expected.Deliveries {
		if expected.Required {
			required[expected.ID] = true
		}
	}
	byStep := map[string][]messagetest.Publication{}
	for _, publication := range report.Published {
		byStep[publication.Step] = append(byStep[publication.Step], publication)
	}
	duplicates := byStep["duplicates"]
	if len(duplicates) != 2 || duplicates[0].Key == duplicates[1].Key {
		t.Fatalf("duplicates published %+v, want two copies under two keys", duplicates)
	}
	if len(byStep["out of order"]) != 2 {
		t.Fatalf("out of order published %+v", byStep["out of order"])
	}
	restartRequired := 0
	for _, publication := range byStep["restart"] {
		if required[publication.ID] {
			restartRequired++
		}
	}
	if restartRequired == 0 {
		t.Errorf("no restart notice is a required delivery: %+v", byStep["restart"])
	}
	for _, publication := range append(append(duplicates, byStep["out of order"]...), byStep["restart"]...) {
		if publication.Step != "restart" && !required[publication.ID] {
			t.Errorf("the %s notice %s is not a required delivery", publication.Step, publication.ID)
		}
		if required[publication.ID] && !arrived[publication.ID] {
			t.Errorf("the %s notice %s is not in the session's transcript", publication.Step, publication.ID)
		}
		if publication.Step == "out of order" && saw[publication.ID] {
			t.Errorf("the out-of-order notice %s has a saw record the owner was told not to make", publication.ID)
		}
		if publication.Step == "duplicates" && !saw[publication.ID] {
			t.Errorf("the duplicate notice %s has no saw record: the report must show the owner's records beside", publication.ID)
		}
	}
	answers := 0
	for _, step := range report.Steps {
		if step.Name == "duplicates" && step.Error != "" {
			t.Errorf("duplicates step failed: %s", step.Error)
		}
	}
	for _, outcome := range report.Expected.Outcomes {
		if outcome.Item == "need:"+ownerpkg.EdgeID(mtFunction, mtStruct)+"@r1" {
			answers++
		}
	}
	if answers != 1 {
		t.Errorf("the duplicated need is expected %d times, want once", answers)
	}
}

// assertTheRunsTmuxServerIsGone checks that the run's tmux server ran under the run's own directory and went with it:
// the report names TMUX_TMPDIR <out>/tmux and the socket in it, the teardown removed that directory, no socket of the
// run is in tmux's default directory (tmux never removes a socket file when its server exits), and no process names
// the socket, as the server's command line does.
func assertTheRunsTmuxServerIsGone(t *testing.T, out, socket string) {
	t.Helper()
	var raw struct {
		Daemon map[string]any `json:"daemon"`
	}
	if data, err := os.ReadFile(filepath.Join(out, "report.json")); err != nil || json.Unmarshal(data, &raw) != nil {
		t.Fatalf("report.json: %v", err)
	}
	dir, uid := filepath.Join(out, "tmux"), "tmux-"+strconv.Itoa(os.Getuid())
	if raw.Daemon["tmux_tmpdir"] != dir || raw.Daemon["tmux_socket_path"] != filepath.Join(dir, uid, socket) {
		t.Errorf("the report's daemon = %v; want its tmux server at %s", raw.Daemon, filepath.Join(dir, uid, socket))
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the run's tmux directory %s is left after the teardown: %v", dir, err)
	}
	defaults := os.Getenv("TMUX_TMPDIR")
	if defaults == "" {
		defaults = "/tmp"
	}
	if _, err := os.Lstat(filepath.Join(defaults, uid, socket)); !os.IsNotExist(err) {
		t.Errorf("the run left the tmux socket %s: %v", filepath.Join(defaults, uid, socket), err)
	}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && entry.Name() != strconv.Itoa(os.Getpid()) && slices.ContainsFunc(bytes.Split(cmdline, []byte{0}), func(arg []byte) bool { return string(arg) == socket }) {
			t.Errorf("process %s names the run's tmux socket after the teardown: %q", entry.Name(), cmdline)
		}
	}
}

// ownerNoticeReport is one of report.json's owner notices, as the end-to-end checks read it.
type ownerNoticeReport struct {
	Step    string `json:"step"`
	Item    string `json:"item"`
	Arrived bool   `json:"arrived"`
}

// assertOwnerNoticesArrived checks, in the source owner's own session transcripts, that the target owner's answer of
// each need published the notice of the offer it queued itself, and that the source's session recorded it: on the
// routing table's offer topic for a data structure (its users topic), from the target, naming the offer. The report
// must show each of them arrived, with the step whose need the answer answered.
func assertOwnerNoticesArrived(t *testing.T, out string, report messagetest.Report) {
	t.Helper()
	users := report.Prefix + "node." + ownerpkg.TopicToken(mtStruct) + ".users"
	var offers []string
	for _, outcome := range report.Expected.Outcomes {
		if outcome.Owner == mtFunction && strings.HasPrefix(outcome.Item, "offer:") {
			offers = append(offers, outcome.Item)
		}
	}
	noticed := map[string]int{}
	for _, pane := range report.Panes {
		if pane.Owner != mtFunction {
			continue
		}
		for _, dir := range pane.Transcripts {
			paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(string(data), "\n") {
					var entry struct {
						CustomType string `json:"customType"`
						Content    string `json:"content"`
						Details    struct {
							Topic   string `json:"topic"`
							Subject string `json:"subject"`
						} `json:"details"`
					}
					if json.Unmarshal([]byte(line), &entry) != nil || entry.CustomType != "envoy-message" {
						continue
					}
					var rendered struct {
						Envoy struct {
							Message struct{ Type, From, Item string } `yaml:"message"`
						} `yaml:"envoy"`
					}
					if yaml.Unmarshal([]byte(entry.Content), &rendered) != nil {
						continue
					}
					if m := rendered.Envoy.Message; m.Type == "offer" && m.From == mtStruct && entry.Details.Topic == users && entry.Details.Subject == users {
						noticed[m.Item]++
					}
				}
			}
		}
	}
	var raw struct {
		OwnerNotices []ownerNoticeReport `json:"owner_notices"`
	}
	if data, err := os.ReadFile(filepath.Join(out, "report.json")); err != nil || json.Unmarshal(data, &raw) != nil {
		t.Fatalf("report.json: %v", err)
	}
	if len(offers) == 0 || len(raw.OwnerNotices) != len(offers) {
		t.Errorf("the run expected the offers %v and reports the owner notices %+v; want one notice of each offer", offers, raw.OwnerNotices)
	}
	steps := []string{"duplicates", "out of order", "kill", "restart"}
	for _, offer := range offers {
		if noticed[offer] == 0 {
			t.Errorf("the source's session never recorded the target's own notice of %s on %s", offer, users)
		}
		if !slices.ContainsFunc(raw.OwnerNotices, func(n ownerNoticeReport) bool {
			return n.Item == offer && n.Arrived && slices.Contains(steps, n.Step)
		}) {
			t.Errorf("the report shows no arrived notice of %s with its step: %+v", offer, raw.OwnerNotices)
		}
	}
}

func sameTopics(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}
