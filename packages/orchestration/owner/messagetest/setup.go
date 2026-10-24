package messagetest

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

//go:embed owner.md
var ownerBrief string

//go:embed copy
var copyFiles embed.FS

// The test's fact: the source owner states it, and the source owner also subscribes to the fact owner's and the
// librarian's topics, which no node owner holds.
const testFact = "stack-alignment"

// TopicKinds are the owner protocol's node topic kinds and the topic every owner holds.
var TopicKinds = []string{"needs", "callers", "callees", "users", "neighbors", "linked", "all"}

// graphInputs are the deployment's graph files the test copies its two nodes from.
type graphInputs struct {
	owners    ownerpkg.Owners
	topics    ownerpkg.Topics
	graph     ownerpkg.Graph
	graphData []byte
}

func readGraph(dir string) (graphInputs, error) {
	owners, err := ownerpkg.LoadOwners(filepath.Join(dir, "owners.json"))
	if err != nil {
		return graphInputs{}, err
	}
	topics, err := ownerpkg.LoadTopics(filepath.Join(dir, "topics.json"))
	if err != nil {
		return graphInputs{}, err
	}
	path := filepath.Join(dir, "graph.json")
	graph, err := ownerpkg.LoadGraph(path)
	if err != nil {
		return graphInputs{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return graphInputs{}, err
	}
	return graphInputs{owners: owners, topics: topics, graph: graph, graphData: data}, nil
}

// topicKind is the last token of a topic: needs, callers, all, and so on.
func topicKind(topic string) string { return topic[strings.LastIndexByte(topic, '.')+1:] }

func coversEveryKind(topics ...[]string) bool {
	held := map[string]bool{}
	for _, list := range topics {
		for _, topic := range list {
			held[topicKind(topic)] = true
		}
	}
	for _, kind := range TopicKinds {
		if !held[kind] {
			return false
		}
	}
	return true
}

// pickNodes chooses the test's two owners when --nodes is not given: the first function, in id order, whose id
// holds `@`, `/` and `.`, with the first data structure it accesses such that the two hold every topic kind. The ids
// then exercise the topic token and the supervisor's owner encoding (C9), and the arrival step reaches every kind.
func (in graphInputs) pickNodes() ([]string, error) {
	kinds := map[string]string{}
	for _, owner := range in.owners.Owners {
		kinds[owner.ID] = owner.Kind
	}
	accessed := map[string][]string{}
	for _, edge := range in.graph.Edges {
		if edge.Kind == "access" && kinds[edge.Target] == "struct" {
			accessed[edge.Source] = append(accessed[edge.Source], edge.Target)
		}
	}
	var functions []string
	for id, kind := range kinds {
		if kind == "function" && strings.Contains(id, "@") && strings.Contains(id, "/") && strings.Contains(id, ".") {
			functions = append(functions, id)
		}
	}
	slices.Sort(functions)
	for _, function := range functions {
		targets := slices.Compact(slices.Sorted(slices.Values(accessed[function])))
		for _, target := range targets {
			if coversEveryKind(in.topics.Owners[function], in.topics.Owners[target]) {
				return []string{function, target}, nil
			}
		}
	}
	return nil, fmt.Errorf("the graph has no function whose id holds @, / and . with a data structure it accesses that together hold every topic kind; pass --nodes")
}

// edge is the test's edge between its two nodes: from the function to the other node, or from the first node when
// both or neither are functions.
func (in graphInputs) edge(nodes []string) (source, target string) {
	first, _ := in.owners.ByID(nodes[0])
	second, _ := in.owners.ByID(nodes[1])
	if second.Kind == "function" && first.Kind != "function" {
		return nodes[1], nodes[0]
	}
	return nodes[0], nodes[1]
}

// write puts the two nodes' graph files in dir: owners.json and topics.json with only them, the topics moved under
// prefix; graph.json unchanged; and facts.json, in which factUser states the test's fact.
func (in graphInputs) write(dir, prefix string, nodes []string, factUser string) (ownerpkg.Topics, ownerpkg.Owners, error) {
	owners := ownerpkg.Owners{Format: in.owners.Format, Part: in.owners.Part}
	topics := ownerpkg.Topics{Format: in.topics.Format, Part: in.topics.Part, Prefix: prefix, TokenRule: in.topics.TokenRule, Owners: map[string][]string{}}
	for _, id := range nodes {
		owner, ok := in.owners.ByID(id)
		if !ok {
			return ownerpkg.Topics{}, ownerpkg.Owners{}, fmt.Errorf("owners.json has no owner %q", id)
		}
		list, ok := in.topics.Owners[id]
		if !ok {
			return ownerpkg.Topics{}, ownerpkg.Owners{}, fmt.Errorf("topics.json has no owner %q", id)
		}
		owners.Owners = append(owners.Owners, owner)
		for _, topic := range list {
			rest, found := strings.CutPrefix(topic, in.topics.Prefix)
			if !found {
				return ownerpkg.Topics{}, ownerpkg.Owners{}, fmt.Errorf("topics.json topic %q is not under its prefix %q", topic, in.topics.Prefix)
			}
			topics.Owners[id] = append(topics.Owners[id], prefix+rest)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ownerpkg.Topics{}, ownerpkg.Owners{}, err
	}
	facts := ownerpkg.Facts{testFact: {Users: []string{factUser}}}
	for name, value := range map[string]any{"owners.json": owners, "topics.json": topics, "facts.json": facts} {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return ownerpkg.Topics{}, ownerpkg.Owners{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o644); err != nil {
			return ownerpkg.Topics{}, ownerpkg.Owners{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "graph.json"), in.graphData, 0o644); err != nil {
		return ownerpkg.Topics{}, ownerpkg.Owners{}, err
	}
	return topics, owners, nil
}

// copyConfig is the test copy's skeleton/owner.yaml. The copy holds no Lean, so its Lean paths are placeholders
// under Skeleton/.
func copyConfig(repo, prefix, graphDir string) ownerpkg.Config {
	return ownerpkg.Config{
		Repo: repo, RecordsRoot: "skeleton", LeanRoot: "Skeleton", TopicPrefix: prefix,
		ActiveFile: "skeleton/active/message-test.yaml", RuleFile: "skeleton/RULE.yaml", ManifestFile: "skeleton/PROTOCOL.yaml",
		FactsFile: "skeleton/facts.yaml", ControlsFile: "skeleton/controls.yaml", GraphDir: graphDir, ScorePart: "slice",
		Paths: ownerpkg.Paths{
			EdgeModuleTemplate: "Skeleton/{key_source}/E{hex16_target}.lean", EdgeTheoremTemplate: "Skeleton.{key_source}.E{hex16_target}.edge",
			InterfaceModuleTemplate: "Skeleton/{key_ds}/I{hex16_interface}.lean", InterfaceTheoremTemplate: "Skeleton.{key_ds}.I{hex16_interface}.edge",
			NodeModuleTemplate: "Skeleton/{key}/Node.lean", EdgeRecordTemplate: "skeleton/nodes/{key_source}/edges/E{hex16_target}.yaml",
			AgreementIDTemplate: "edge:{source}->{target}@{n}",
		},
		Build: ownerpkg.BuildTargets{GateTargets: []string{"Skeleton"}},
	}
}

// createCopy makes the test's bare repository at bare: main holds skeleton/owner.yaml, the manifest, the rule, the
// facts and the active set of the two nodes. Owner commits use the repository's own identity.
func createCopy(bare, scratch string, config ownerpkg.Config, nodes []string) error {
	files := map[string][]byte{}
	for _, name := range []string{"PROTOCOL.yaml", "RULE.yaml", "facts.yaml"} {
		data, err := copyFiles.ReadFile("copy/" + name)
		if err != nil {
			return err
		}
		files["skeleton/"+name] = data
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	files[ownerpkg.ConfigPath] = data
	active, err := yaml.Marshal(struct {
		Name  string   `yaml:"name"`
		Nodes []string `yaml:"nodes"`
	}{"message-test", nodes})
	if err != nil {
		return err
	}
	files[config.ActiveFile] = active
	for path, data := range files {
		full := filepath.Join(scratch, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
	}
	identity := []string{"-c", "user.name=owner message test", "-c", "user.email=message-test@owner.invalid"}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", scratch},
		{"-C", scratch, "add", "."},
		append(append([]string{"-C", scratch}, identity...), "commit", "-q", "-m", "skeleton: the message test's copy"),
		{"clone", "-q", "--bare", scratch, bare},
		{"--git-dir=" + bare, "config", "user.name", "owner message test"},
		{"--git-dir=" + bare, "config", "user.email", "message-test@owner.invalid"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
	}
	return os.RemoveAll(scratch)
}

// renderBrief is owner.md with the owner command of this test's deployment.
func renderBrief(owner, deploy string) ([]byte, error) {
	brief, err := template.New("owner.md").Parse(ownerBrief)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = brief.Execute(&out, struct{ Command string }{owner + " --deploy " + deploy})
	return out.Bytes(), err
}

// DaemonConfig is the supervisor's minimal standing-owners configuration (shared contract 10, C33): no software
// workflow keys, the daemon's own Postgres from LEGION_POSTGRES_DSN in its environment, the operator bearer that the
// daemon requires at boot, and one template for each kind of the test's nodes. The omp the panes run is no key: the
// driver gives it as LEGION_OMP_PATH in the supervisor's environment.
type DaemonConfig struct {
	Project           string         `yaml:"project"`
	StateDir          string         `yaml:"state_dir"`
	Runtime           string         `yaml:"runtime"`
	Port              int            `yaml:"port"`
	OperatorTokenFile string         `yaml:"operator_token_file"`
	EnvoyURL          string         `yaml:"envoy_url,omitempty"`
	NATSURLs          []string       `yaml:"nats_urls,omitempty"`
	EnvoyTokenFile    string         `yaml:"envoy_token_file,omitempty"`
	StandingOwners    StandingOwners `yaml:"standing_owners"`
}

// StandingOwners is the daemon's standing_owners block: the ledger, the repository and limits, and the templates
// that start a graph owner by its kind (C7, C33).
type StandingOwners struct {
	LedgerDSNFile string                   `yaml:"ledger_dsn_file"`
	Repository    string                   `yaml:"repository"`
	MaxRunning    int                      `yaml:"max_running"`
	BudgetUSD     float64                  `yaml:"budget_usd"`
	Templates     map[string]OwnerTemplate `yaml:"templates"`
}

// OwnerTemplate starts every owner of one node kind.
type OwnerTemplate struct {
	PromptFile string  `yaml:"prompt_file"`
	Harness    string  `yaml:"harness"`
	MaxCostUSD float64 `yaml:"max_cost_usd"`
}

// The test's safety caps: high, against a runaway loop, not a limit on the run.
const (
	ownerMaxCostUSD = 20
	daemonBudgetUSD = 1000
)

func daemonConfig(project, stateDir string, port int, envoyURL string, natsURLs []string, tokenFile, operatorTokenFile, ledgerDSNFile, repo, prompt string, kinds []string) DaemonConfig {
	templates := map[string]OwnerTemplate{}
	for _, kind := range kinds {
		templates[kind] = OwnerTemplate{PromptFile: prompt, Harness: "omp", MaxCostUSD: ownerMaxCostUSD}
	}
	return DaemonConfig{
		Project: project, StateDir: stateDir, Runtime: "tmux", Port: port, OperatorTokenFile: operatorTokenFile,
		EnvoyURL: envoyURL, NATSURLs: natsURLs, EnvoyTokenFile: tokenFile,
		StandingOwners: StandingOwners{LedgerDSNFile: ledgerDSNFile, Repository: repo, MaxRunning: 2, BudgetUSD: daemonBudgetUSD, Templates: templates},
	}
}

// ProjectToken is the daemon's spelling of a project in its tmux socket and tokens: lowercase, letters and digits.
func ProjectToken(project string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(project) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// DaemonSocket is the tmux socket (-L) of the daemon of project.
func DaemonSocket(project string) string { return "legion-" + ProjectToken(project) }

// daemonProject is the project of a run's daemon: mt and the run's 8 random hex digits. The project names the
// daemon's tmux socket, legion-<project>, which lives under the run's directory and must fit a Unix socket's path, so
// it is short; two runs of one second draw other digits, so they have other projects. The ledger repository, the
// topics and the bearer directory keep the run's full name.
func daemonProject(run string) string { return "mt" + run[strings.LastIndexByte(run, '-')+1:] }
