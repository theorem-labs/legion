package messagetest

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

const source = "notifications.example."

// writeGraphDir writes a deployment graph: owners, their topics under notifications.example., and access edges.
func writeGraphDir(t *testing.T, owners map[string]string, topics map[string][]string, access [][2]string) string {
	t.Helper()
	dir := t.TempDir()
	var ownerList []map[string]any
	for _, id := range slices.Sorted(maps.Keys(owners)) {
		role := map[string]string{"function": "function", "struct": "data_structure"}[owners[id]]
		ownerList = append(ownerList, map[string]any{"id": id, "kind": owners[id], "role": role, "name": id})
	}
	var edges []map[string]any
	for _, edge := range access {
		edges = append(edges, map[string]any{"kind": "access", "source": edge[0], "target": edge[1], "access": "read"})
	}
	prefixed := map[string][]string{}
	for id, list := range topics {
		for _, topic := range list {
			prefixed[id] = append(prefixed[id], source+topic)
		}
	}
	for name, value := range map[string]any{
		"owners.json": map[string]any{"format": "cgraph/owners/v1", "part": "slice", "owners": ownerList},
		"topics.json": map[string]any{"format": "cgraph/topics/v1", "part": "slice", "prefix": source, "token_rule": "rule", "owners": prefixed},
		"graph.json":  map[string]any{"format": "cgraph/graph/v1", "part": "slice", "nodes": []any{}, "edges": edges},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func everyKind(self, other string) []string {
	return []string{"all", "node." + self + ".needs", "node.function:g.callers", "node.function:h.callees",
		"node." + other + ".users", "node." + other + ".neighbors"}
}

func TestPickNodesChoosesAFunctionWithC9BytesAndAStructTheyHoldEveryKindWith(t *testing.T) {
	dir := writeGraphDir(t,
		map[string]string{"function:a-plain": "function", "function:b@x/y.c": "function", "function:c@x/y.c": "function", "struct:s": "struct", "struct:t": "struct"},
		map[string][]string{
			"function:a-plain": append(everyKind("function:a-plain", "struct:t"), "node.struct:u.linked"),
			"function:b@x/y.c": everyKind("function:b%40x%2Fy%2Ec", "struct:s"),
			"function:c@x/y.c": everyKind("function:c%40x%2Fy%2Ec", "struct:t"),
			"struct:s":         {"all", "node.struct:s.needs"},
			"struct:t":         {"all", "node.struct:t.needs", "node.struct:u.linked"},
		},
		[][2]string{{"function:a-plain", "struct:t"}, {"function:b@x/y.c", "struct:s"}, {"function:c@x/y.c", "struct:t"}})
	graph, err := readGraph(dir)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := graph.pickNodes()
	if err != nil || !slices.Equal(nodes, []string{"function:c@x/y.c", "struct:t"}) {
		t.Fatalf("pickNodes = %v, %v", nodes, err)
	}
	if source, target := graph.edge([]string{"struct:t", "function:c@x/y.c"}); source != "function:c@x/y.c" || target != "struct:t" {
		t.Fatalf("edge = %s -> %s, want the function first", source, target)
	}

	dir = writeGraphDir(t, map[string]string{"function:b@x/y.c": "function", "struct:s": "struct"},
		map[string][]string{"function:b@x/y.c": {"all"}, "struct:s": {"all"}}, [][2]string{{"function:b@x/y.c", "struct:s"}})
	if graph, err = readGraph(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.pickNodes(); err == nil || !strings.Contains(err.Error(), "--nodes") {
		t.Fatalf("pickNodes with no candidate error = %v", err)
	}
}

func TestGraphWriteKeepsTheTwoNodesUnderThePrefix(t *testing.T) {
	fn, st := "function:c@x/y.c", "struct:t"
	dir := writeGraphDir(t, map[string]string{fn: "function", st: "struct", "struct:other": "struct"},
		map[string][]string{fn: everyKind("function:c%40x%2Fy%2Ec", st), st: {"all", "node.struct:t.needs"}, "struct:other": {"all"}},
		[][2]string{{fn, st}})
	graph, err := readGraph(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "graph")
	prefix := "notifications.example-message-test."
	if _, _, err := graph.write(out, prefix, []string{fn, st}, fn); err != nil {
		t.Fatal(err)
	}
	topics, err := ownerpkg.LoadTopics(filepath.Join(out, "topics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if topics.Prefix != prefix || len(topics.Owners) != 2 || !slices.Equal(topics.Owners[st], []string{prefix + "all", prefix + "node.struct:t.needs"}) {
		t.Fatalf("topics.json = %+v", topics)
	}
	owners, err := ownerpkg.LoadOwners(filepath.Join(out, "owners.json"))
	if err != nil || len(owners.Owners) != 2 {
		t.Fatalf("owners.json = %+v, %v", owners, err)
	}
	facts, err := ownerpkg.LoadFacts(filepath.Join(out, "facts.json"))
	if err != nil || !slices.Equal(facts[testFact].Users, []string{fn}) {
		t.Fatalf("facts.json = %+v, %v", facts, err)
	}
	original, _ := os.ReadFile(filepath.Join(dir, "graph.json"))
	copied, _ := os.ReadFile(filepath.Join(out, "graph.json"))
	if !bytes.Equal(original, copied) {
		t.Fatal("graph.json changed")
	}
	if _, _, err := graph.write(out, prefix, []string{fn, "struct:missing"}, fn); err == nil {
		t.Fatal("write accepted a node the graph does not have")
	}
}

// setupStopOptions are the options of a run of three kills on a graph of fn and st under prefix (empty: the graph's
// own). The deployment names only its graph and a ledger DSN file that does not exist, so the run stops in setup once
// it has written the test graph, the test copy and the driver's extra topics.
func setupStopOptions(t *testing.T, prefix string) Options {
	t.Helper()
	fn, st := "function:c@x/y.c", "struct:t"
	dir := writeGraphDir(t, map[string]string{fn: "function", st: "struct"},
		map[string][]string{fn: everyKind("function:c%40x%2Fy%2Ec", st), st: {"all", "node.struct:t.needs", "node.struct:u.linked"}}, [][2]string{{fn, st}})
	return Options{
		Prefix: prefix, Nodes: []string{fn, st}, Kills: 3, Out: filepath.Join(t.TempDir(), "run"),
		Deploy: ownerpkg.Deploy{GraphDir: dir, LedgerDSNFile: filepath.Join(t.TempDir(), "ledger-dsn")},
		Owner:  "/opt/bin/owner", Legion: "/opt/bin/legion",
		Getenv: func(string) string { return "" }, Progress: io.Discard, Timeouts: Timeouts{Poll: time.Millisecond},
	}
}

// runUntilSetupStops runs the message test of setupStopOptions until its setup stops.
func runUntilSetupStops(t *testing.T, prefix string) (Report, string) {
	t.Helper()
	opts := setupStopOptions(t, prefix)
	report, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return report, opts.Out
}

// A full run kills one owner pane at each of the three moments of a step, so Run refuses an explicit count of one or
// two kills, before it makes its output directory (0 is the default schedule).
func TestRunRefusesFewerKillsThanMoments(t *testing.T) {
	for _, kills := range []int{1, 2} {
		opts := setupStopOptions(t, "")
		opts.Kills = kills
		if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "at least 3") {
			t.Errorf("Run with %d kills: %v, want a refusal that asks for at least 3", kills, err)
		}
		if _, err := os.Stat(opts.Out); !os.IsNotExist(err) {
			t.Errorf("a refused run made its output directory %s: %v", opts.Out, err)
		}
	}
}

// setupFailure is the error of a run's setup step, or "" when it had none.
func setupFailure(report Report) string {
	for _, step := range report.Steps {
		if step.Name == "setup" {
			return step.Error
		}
	}
	return ""
}

// The arrival step publishes one notice on each topic kind the two nodes hold. A kind that no topic of the nodes has
// fails the run in its setup, before any daemon or owner session runs, unless --allow-missing-kinds names it; the
// report then lists it. A name that is no topic kind is refused.
func TestRunFailsOnATopicKindTheNodesLackUnlessItIsAllowed(t *testing.T) {
	fn, st := "function:c@x/y.c", "struct:t"
	noLinked := writeGraphDir(t, map[string]string{fn: "function", st: "struct"},
		map[string][]string{fn: everyKind("function:c%40x%2Fy%2Ec", st), st: {"all", "node.struct:t.needs"}}, [][2]string{{fn, st}})
	run := func(allowed ...string) Report {
		opts := setupStopOptions(t, "")
		opts.Deploy.GraphDir, opts.AllowMissingKinds = noLinked, allowed
		report, err := Run(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	report := run()
	if failure := setupFailure(report); !strings.Contains(failure, "linked") || !strings.Contains(failure, "--allow-missing-kinds") ||
		len(report.AllowedMissingKinds) != 0 || report.Passed {
		t.Fatalf("a run whose nodes hold no linked topic: setup %q, allowed missing %v, passed %t", failure, report.AllowedMissingKinds, report.Passed)
	}
	if _, err := os.Stat(filepath.Join(report.Out, "deploy.yaml")); !os.IsNotExist(err) {
		t.Fatalf("the setup went on past the missing kind and wrote the test deployment: %v", err)
	}

	report = run("linked", "all")
	if failure := setupFailure(report); strings.Contains(failure, "linked") || !slices.Equal(report.AllowedMissingKinds, []string{"linked"}) {
		t.Fatalf("a run that allows linked: setup %q, allowed missing %v; want linked listed and no failure of it", failure, report.AllowedMissingKinds)
	}
	if _, err := os.Stat(filepath.Join(report.Out, "deploy.yaml")); err != nil {
		t.Fatalf("a run that allows linked stopped at the kinds: %v", err)
	}
	if markdown, err := os.ReadFile(filepath.Join(report.Out, "report.md")); err != nil || !strings.Contains(string(markdown), "`--allow-missing-kinds`: `linked`") {
		t.Fatalf("report.md does not list the allowed missing kind: %v\n%s", err, markdown)
	}

	opts := setupStopOptions(t, "")
	opts.AllowMissingKinds = []string{"linkd"}
	if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "linkd") {
		t.Fatalf("Run allowing the missing kind linkd: %v, want a refusal that names it", err)
	}
}

// assertRunPrefix checks that every topic of the run is under want: the report, the test graph's topics.json and every
// topic in it, the test copy's topic_prefix, and the fact and concept topics the driver gives the source owner.
func assertRunPrefix(t *testing.T, report Report, out, want string) {
	t.Helper()
	if report.Prefix != want {
		t.Fatalf("report prefix = %q, want %q", report.Prefix, want)
	}
	topics, err := ownerpkg.LoadTopics(filepath.Join(out, "graph", "topics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if topics.Prefix != want || !slices.Equal(topics.Owners["struct:t"], []string{want + "all", want + "node.struct:t.needs", want + "node.struct:u.linked"}) {
		t.Fatalf("topics.json = %+v, want every topic under %q", topics, want)
	}
	config, err := ownerpkg.LoadConfig(&ownerpkg.Repo{Path: filepath.Join(out, "repo.git")})
	if err != nil || config.TopicPrefix != want {
		t.Fatalf("the test copy's configuration = %+v, %v; want topic_prefix %q", config, err, want)
	}
	extra, err := LoadExtraTopics(filepath.Join(out, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if got := extra["function:c@x/y.c"]; !slices.Equal(got, []string{want + "fact.stack-alignment.needs", want + "concepts"}) {
		t.Fatalf("the source owner's extra topics = %v, want them under %q", got, want)
	}
}

// A run's topics are under the --prefix it is given and then its own name, so runs on one hub, with one --prefix or
// started in the same second, never see each other's notices.
func TestRunPutsItsTopicsUnderItsPrefixAndItsName(t *testing.T) {
	report, out := runUntilSetupStops(t, "notifications.example-message-test.")
	assertRunPrefix(t, report, out, "notifications.example-message-test."+report.Run+".")
}

// With no prefix given, a run's topics are under the deployment graph's own prefix, the one the deployment's
// configuration names, and then its own name; the code holds no default prefix of its own.
func TestRunWithNoPrefixPutsItsTopicsUnderTheGraphsPrefixAndItsName(t *testing.T) {
	report, out := runUntilSetupStops(t, "")
	assertRunPrefix(t, report, out, source+report.Run+".")
}

// The supervisor's configuration is the minimal standing-owners file of C33, with the operator bearer the daemon
// requires at boot: no software-workflow keys, the daemon's database from LEGION_POSTGRES_DSN, two running owners,
// and one omp template per node kind.
func TestDaemonConfigIsTheMinimalStandingOwnersFile(t *testing.T) {
	config := daemonConfig("message-test-x", "/run/daemon/state", 14000, "http://127.0.0.1:9020", []string{"nats://hub:4222"},
		"/run/daemon/envoy-token", "/srv/state/message-test/x/operator-token", "/srv/ledger-dsn", "message-test-x", "/run/daemon/owner.md", []string{"function", "struct"})
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
	if got := keys(decoded); !slices.Equal(got, []string{"envoy_token_file", "envoy_url", "nats_urls", "operator_token_file", "port", "project", "runtime", "standing_owners", "state_dir"}) {
		t.Fatalf("top-level keys = %v", got)
	}
	owners := decoded["standing_owners"].(map[string]any)
	if got := keys(owners); !slices.Equal(got, []string{"budget_usd", "ledger_dsn_file", "max_running", "repository", "templates"}) {
		t.Fatalf("standing_owners keys = %v", got)
	}
	if owners["max_running"] != 2 || owners["repository"] != "message-test-x" || decoded["runtime"] != "tmux" || decoded["operator_token_file"] != "/srv/state/message-test/x/operator-token" {
		t.Fatalf("config = %v", decoded)
	}
	templates := owners["templates"].(map[string]any)
	if got := keys(templates); !slices.Equal(got, []string{"function", "struct"}) {
		t.Fatalf("templates = %v", got)
	}
	for kind, value := range templates {
		template := value.(map[string]any)
		if template["prompt_file"] != "/run/daemon/owner.md" || template["harness"] != "omp" || template["max_cost_usd"] == nil {
			t.Fatalf("template %s = %v", kind, template)
		}
	}
}

func TestDaemonSocketIsTheProjectsTmuxServer(t *testing.T) {
	if got := DaemonSocket("message-test-20261023t180000z-0a1b2c3d"); got != "legion-messagetest20261023t180000z0a1b2c3d" {
		t.Fatalf("DaemonSocket = %q", got)
	}
}

// A run's name is its UTC stamp and a random suffix, so runs started in the same second differ. It names the ledger
// repository, the daemon's project, the tmux socket, the private bearer directory and a token of every topic, so it
// holds only lowercase letters, digits and hyphens: one topic token, one path element, and a project token that
// keeps the suffix.
func TestRunNameIsItsStampAndARandomSuffix(t *testing.T) {
	at := time.Date(2026, 10, 24, 5, 4, 20, 999, time.UTC)
	names := map[string]bool{}
	for range 100 {
		name, err := runName(at)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^message-test-20261024t050420z-[0-9a-f]{8}$`).MatchString(name) {
			t.Fatalf("run name %q is not message-test-<stamp>-<8 hex digits>", name)
		}
		if names[DaemonSocket(name)] {
			t.Fatalf("two runs of one second share the tmux socket %s", DaemonSocket(name))
		}
		names[DaemonSocket(name)] = true
	}
}

// The driver expects the offer that the target's answer queues by production's rule: the n-th need the target answers
// rewrites its answer record as revision n, which queues offer:<edge>@r<n>, whatever the need's own version.
func TestDriverExpectsOffersNumberedByTheAnswerRevision(t *testing.T) {
	d := &driver{source: "function:a@x/y.c", target: "struct:t"}
	for _, version := range []string{"r1", "r3", "r4"} {
		d.expectAnswered(version)
	}
	edge := ownerpkg.EdgeID(d.source, d.target)
	var followUps, offers []string
	for _, outcome := range d.expected.Outcomes {
		if outcome.FollowUp != "" {
			followUps = append(followUps, outcome.FollowUp)
		}
		if outcome.Owner == d.source {
			offers = append(offers, outcome.Item)
		}
	}
	want := []string{"offer:" + edge + "@r1", "offer:" + edge + "@r2", "offer:" + edge + "@r3"}
	if !slices.Equal(followUps, want) || !slices.Equal(offers, want) {
		t.Fatalf("follow-ups %v and offer outcomes %v, want %v", followUps, offers, want)
	}
}

func TestBriefNamesTheTestDeploymentsOwnerCommand(t *testing.T) {
	brief, err := renderBrief("/opt/bin/owner", "/runs/one/deploy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(brief)
	for _, want := range []string{
		"Your owner command is: `/opt/bin/owner --deploy /runs/one/deploy.yaml`", "message-test topics", "envoy_subscribe",
		// An owner of many topics once subscribed with the one wildcard <prefix>.> (run accept-gate-3), which takes
		// every other owner's notices too: the brief names the exact list and forbids a wildcard.
		"exactly as printed", "never a wildcard",
		"message-test subscribed", "--json inbox", "claim <item>", "message-test answer <item>", "message-test saw <id> <topic>", "End the turn",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("owner.md does not say %q", want)
		}
	}
}

func TestCreateCopyIsAnOwnerConfigurationAtMain(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	prefix := "notifications.example-message-test."
	if err := createCopy(bare, filepath.Join(root, "seed"), copyConfig("message-test-x", prefix, filepath.Join(root, "graph")), []string{"function:c@x/y.c", "struct:t"}); err != nil {
		t.Fatal(err)
	}
	repo := ownerpkg.Repo{Path: bare}
	config, err := ownerpkg.LoadConfig(&repo)
	if err != nil {
		t.Fatal(err)
	}
	if config.Repo != "message-test-x" || config.TopicPrefix != prefix {
		t.Fatalf("config = %+v", config)
	}
	active, err := repo.Show(t.Context(), "main", config.ActiveFile)
	if err != nil || !strings.Contains(string(active), "function:c@x/y.c") {
		t.Fatalf("active set = %q, %v", active, err)
	}
	for _, file := range []string{config.RuleFile, config.ManifestFile, config.FactsFile} {
		if _, err := repo.Show(t.Context(), "main", file); err != nil {
			t.Fatalf("main has no %s: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "seed")); !os.IsNotExist(err) {
		t.Fatalf("the seed checkout is left: %v", err)
	}
}

// The report records what ran: the owner's build and Go modules, the supervisor's and omp's own version output,
// tmux and git, and a digest of each graph input. A tool that cannot say is recorded as such, never omitted.
func TestCollectRevisionsRecordsEachExecutableAndGraphInput(t *testing.T) {
	graphDir := writeGraphDir(t, map[string]string{"function:b@x/y.c": "function", "struct:s": "struct"},
		map[string][]string{"function:b@x/y.c": {"all"}, "struct:s": {"all"}}, [][2]string{{"function:b@x/y.c", "struct:s"}})
	bin := t.TempDir()
	script := func(name, flag, output string) string {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = "+flag+" ] && echo '"+output+"' && exit 0\necho \"unknown $1\" >&2\nexit 2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	legion := script("legion", "version", "legion v9 (fedcba9876543210)")
	omp := script("omp", "--version", "omp 1.57.0")
	revisions := collectRevisions(t.Context(), legion, omp, graphDir)
	if revisions.Legion != "legion v9 (fedcba9876543210)" || revisions.Omp != "omp 1.57.0" {
		t.Fatalf("legion %q, omp %q", revisions.Legion, revisions.Omp)
	}
	if revisions.Owner.GoVersion == "" || !strings.HasPrefix(revisions.Git, "git version") || !strings.Contains(revisions.Tmux, "tmux") {
		t.Fatalf("revisions = %+v", revisions)
	}
	for _, name := range []string{"graph.json", "owners.json", "topics.json"} {
		data, err := os.ReadFile(filepath.Join(graphDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if revisions.Graph[name] != ownerpkg.ContentHash(data) {
			t.Errorf("graph digest of %s = %q, want %q", name, revisions.Graph[name], ownerpkg.ContentHash(data))
		}
	}
	missing := collectRevisions(t.Context(), legion, filepath.Join(bin, "no-omp"), graphDir)
	if !strings.HasPrefix(missing.Omp, "unavailable: ") {
		t.Fatalf("omp that cannot run = %q, want it recorded as unavailable", missing.Omp)
	}
}

// The supervisor reads the Envoy bearer and its operator bearer from files. A token that came from the environment or
// the Envoy configuration, and the operator bearer the run makes, are written to private files outside the run's
// output directory, never copied into it.
func TestDaemonTokenFilesNeverLandInTheOutputDirectory(t *testing.T) {
	private := filepath.Join(t.TempDir(), "state", "message-test", "run")
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if err := privateDir(private); err != nil {
		t.Fatal(err)
	}
	if dirInfo, err := os.Stat(private); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("token directory %s: %v, %v", private, dirInfo, err)
	}

	path, err := daemonTokenFile(env(nil), "secret-token", private)
	if err != nil || filepath.Dir(path) != private {
		t.Fatalf("daemonTokenFile = %q, %v; want a new file in %s", path, err, private)
	}
	operator, err := operatorTokenFile(private)
	if err != nil || filepath.Dir(operator) != private || operator == path {
		t.Fatalf("operatorTokenFile = %q, %v; want its own file in %s", operator, err, private)
	}
	for _, file := range []string{path, operator} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("token file %s: %v, %v", file, info, err)
		}
	}
	if data, err := os.ReadFile(operator); err != nil || len(strings.TrimSpace(string(data))) != 64 {
		t.Fatalf("operator bearer %q, %v; want 32 random bytes in hex", data, err)
	}

	if path, err := daemonTokenFile(env(map[string]string{"ENVOY_TOKEN_FILE": "/run/secrets/envoy"}), "secret-token", private); err != nil || path != "/run/secrets/envoy" {
		t.Fatalf("with ENVOY_TOKEN_FILE: %q, %v", path, err)
	}
	if path, err := daemonTokenFile(env(nil), "", private); err != nil || path != "" {
		t.Fatalf("with no token: %q, %v", path, err)
	}
}

// The daemon never looks omp up on PATH, so the driver names the omp the panes run as the supervisor's
// LEGION_OMP_PATH: the operator's LEGION_OMP_PATH, else the omp on the supervisor's PATH, and never a relative path or
// a file that cannot run.
func TestOmpPathIsNamedToTheSupervisor(t *testing.T) {
	executable := func(dir, name string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	bin, builds := t.TempDir(), t.TempDir()
	onPath := executable(bin, "omp", 0o755)
	named := executable(builds, "omp-build", 0o755)
	plain := executable(builds, "omp-text", 0o644)
	driverWith := func(path string, env map[string]string) *driver {
		return &driver{opts: Options{Owner: filepath.Join(t.TempDir(), "owner"), Environ: []string{"PATH=" + path, "LEGION_OMP_PATH=/stale/omp"},
			Getenv: func(name string) string { return env[name] }}}
	}

	d := driverWith(bin, nil)
	if got, err := d.ompPath(); err != nil || got != onPath {
		t.Fatalf("ompPath with omp on PATH = %q, %v; want %s", got, err, onPath)
	}
	d = driverWith(bin, map[string]string{"LEGION_OMP_PATH": named})
	got, err := d.ompPath()
	if err != nil || got != named {
		t.Fatalf("ompPath with LEGION_OMP_PATH = %q, %v; want %s", got, err, named)
	}
	d.omp = got
	var given []string
	for _, entry := range d.daemonEnv() {
		if value, ok := strings.CutPrefix(entry, "LEGION_OMP_PATH="); ok {
			given = append(given, value)
		}
	}
	if !slices.Equal(given, []string{named}) {
		t.Fatalf("the supervisor's LEGION_OMP_PATH = %v, want only %s", given, named)
	}
	for name, env := range map[string]map[string]string{
		"a relative LEGION_OMP_PATH": {"LEGION_OMP_PATH": "omp"},
		"a file that cannot run":     {"LEGION_OMP_PATH": plain},
		"no omp anywhere":            nil,
	} {
		path := bin
		if env == nil {
			path = builds
		}
		if got, err := driverWith(path, env).ompPath(); err == nil {
			t.Errorf("ompPath with %s = %q, want a refusal", name, got)
		}
	}
}
