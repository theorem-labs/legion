package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// The message test runs end to end as `owner message-test`: real subprocesses, a real tmux server, the ledger on
// real Postgres and an HTTP listener. The supervisor and the sessions are internal/ownerharness's test-only
// stand-ins (linked as legion and omp), so this run checks the driver and the owner command, and is no evidence
// that the Legion daemon meets C8, C9 or C33.
func TestMessageTestRunsEndToEndAgainstTheSupervisorHarness(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the message test's owners run in a real tmux server")
	}
	root := t.TempDir()
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
	defer listener.Close()
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(bin, "owner"), "--deploy", deploy, "message-test",
		"--prefix", "notifications.example-message-test.", "--kills", "3", "--out", out)
	// The driver reads its ledger from the deployment's ledger_dsn_file alone: a launch variable in the operator's
	// shell names no ledger of the run.
	command.Env = append(os.Environ(), "PATH="+bin+string(filepath.ListSeparator)+os.Getenv("PATH"), "HOME="+root,
		"ENVOY_URL="+listener.URL, "ENVOY_TOKEN=message-test-token", "LEGION_POSTGRES_DSN=postgres://legion@127.0.0.1/legion",
		"OWNER_HARNESS_STEP_DELAY=300ms", "LEGION_LEDGER_DSN_FILE="+filepath.Join(root, "not-the-deployment-ledger"))
	output, err := command.CombinedOutput()
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
	for _, step := range []string{"setup", "start the daemon", "wake from the ledger", "arrival", "duplicates", "out of order", "kill 1 of 3", "daemon restart", "collect", "teardown"} {
		if !slices.ContainsFunc(report.Steps, func(s messagetest.StepTiming) bool { return s.Name == step && s.Error == "" }) {
			t.Errorf("step %q did not run cleanly: %+v", step, report.Steps)
		}
	}
	if err := exec.Command("tmux", "-L", report.Daemon.Socket, "list-sessions").Run(); err == nil {
		t.Fatalf("the daemon's tmux server %s still runs after the teardown", report.Daemon.Socket)
	}
	if !strings.Contains(string(output), "report "+filepath.Join(out, "report.md")) {
		t.Fatalf("the command did not print the report path:\n%s", output)
	}
	assertDuplicatesAndOrderRecorded(t, report)

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

// A notice delivered twice under two keys is recorded twice and answered once; the out-of-order notices and the
// restart's notice to its running owner are required deliveries, each recorded.
func assertDuplicatesAndOrderRecorded(t *testing.T, report messagetest.Report) {
	t.Helper()
	recorded := map[string]bool{}
	for _, delivery := range report.Deliveries {
		if delivery.Owner == mtStruct {
			recorded[delivery.ID] = true
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
		if required[publication.ID] && !recorded[publication.ID] {
			t.Errorf("the %s notice %s was not recorded", publication.Step, publication.ID)
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
