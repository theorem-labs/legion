package owner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the owner protocol configuration stored in the copy's skeleton/owner.yaml.
type Config struct {
	Repo         string       `yaml:"repo"`
	RecordsRoot  string       `yaml:"records_root"`
	LeanRoot     string       `yaml:"lean_root"`
	TopicPrefix  string       `yaml:"topic_prefix"`
	ActiveFile   string       `yaml:"active"`
	RuleFile     string       `yaml:"rule"`
	ManifestFile string       `yaml:"manifest"`
	FactsFile    string       `yaml:"facts"`
	ControlsFile string       `yaml:"controls"`
	GraphDir     string       `yaml:"graph_dir"`
	ScorePart    string       `yaml:"score_part"`
	Paths        Paths        `yaml:"paths"`
	Build        BuildTargets `yaml:"build"`
}

// BuildTargets names the copy targets an owner can ask the build VM to check.
type BuildTargets struct {
	GateTargets []string `yaml:"gate_targets"`
}

// Deploy is the operator-owned owner deployment configuration.
type Deploy struct {
	BareRepo         string          `yaml:"bare_repo"`
	LandingRepo      string          `yaml:"landing_repo"`
	LedgerDSNFile    string          `yaml:"ledger_dsn_file"`
	VMHost           string          `yaml:"vm_host"`
	SSHConfig        string          `yaml:"ssh_config"`
	VMRoot           string          `yaml:"vm_root"`
	GraphDir         string          `yaml:"graph_dir"`
	StateDir         string          `yaml:"state_dir"`
	SessionsDir      string          `yaml:"sessions_dir"`
	ScoreWork        string          `yaml:"score_work"`
	LedgerRemotePort int             `yaml:"ledger_remote_port"`
	LedgerLocalPort  int             `yaml:"ledger_local_port"`
	Hooks            map[string]Hook `yaml:"hooks"`
	Upstream         UpstreamConfig  `yaml:"upstream"`
}

// Hook is one host-produced input to a post-landing hook.
type Hook struct {
	Dir     string   `yaml:"dir"`
	Command string   `yaml:"command"`
	Ledger  bool     `yaml:"ledger"`
	Parts   []string `yaml:"parts"`
}

// UpstreamConfig configures the upstream copy and its cache location.
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	Branch      string `yaml:"branch"`
	CacheHost   string `yaml:"cache_host"`
	CacheDir    string `yaml:"cache_dir"`
	PollSeconds string `yaml:"poll_seconds"`
}

// ConfigPath is the owner protocol configuration's path in the copy.
const ConfigPath = "skeleton/owner.yaml"

// LoadConfig reads skeleton/owner.yaml at main of repo and refuses unknown or missing keys.
func LoadConfig(repo *Repo) (Config, error) {
	data, err := repo.Show(context.Background(), "main", ConfigPath)
	if err != nil {
		return Config{}, fmt.Errorf("read main:%s: %w", ConfigPath, err)
	}
	var config Config
	if err := decodeStrictYAML(data, &config); err != nil {
		return Config{}, fmt.Errorf("decode main:%s: %w", ConfigPath, err)
	}
	if err := config.validate(); err != nil {
		return Config{}, fmt.Errorf("main:%s: %w", ConfigPath, err)
	}
	return config, nil
}

func (c Config) validate() error {
	for _, field := range []struct{ name, value string }{
		{"repo", c.Repo}, {"records_root", c.RecordsRoot}, {"lean_root", c.LeanRoot}, {"topic_prefix", c.TopicPrefix},
		{"active", c.ActiveFile}, {"rule", c.RuleFile}, {"manifest", c.ManifestFile}, {"facts", c.FactsFile},
		{"controls", c.ControlsFile}, {"graph_dir", c.GraphDir}, {"score_part", c.ScorePart},
		{"paths.edge_module", c.Paths.EdgeModuleTemplate}, {"paths.edge_theorem", c.Paths.EdgeTheoremTemplate},
		{"paths.interface_module", c.Paths.InterfaceModuleTemplate}, {"paths.interface_theorem", c.Paths.InterfaceTheoremTemplate},
		{"paths.node_module", c.Paths.NodeModuleTemplate}, {"paths.edge_record", c.Paths.EdgeRecordTemplate},
		{"paths.agreement_id", c.Paths.AgreementIDTemplate},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("missing %s", field.name)
		}
	}
	if !ValidTopicPrefix(c.TopicPrefix) {
		return fmt.Errorf("topic_prefix %q must be notifications.lxspec. or a test run's notifications.lxspec-<name>., and end with a dot", c.TopicPrefix)
	}
	if len(c.Build.GateTargets) == 0 {
		return errors.New("missing build.gate_targets")
	}
	return nil
}

// topicPrefix is the protocol's prefix, notifications.lxspec., or a test run's notifications.lxspec-<name>.,
// optionally followed by more tokens; it ends in a dot.
var topicPrefix = regexp.MustCompile(`^notifications\.lxspec(-[a-z0-9]+)*\.([^.\s]+\.)*$`)

// ValidTopicPrefix reports whether prefix can start owner topics: notifications.lxspec., or a test run's
// notifications.lxspec-<name>. (lowercase letters, digits and hyphens), which keeps its traffic away from owners.
func ValidTopicPrefix(prefix string) bool { return topicPrefix.MatchString(prefix) }

// LoadDeploy reads an operator deployment file and refuses unknown or missing required keys.
func LoadDeploy(path string) (Deploy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Deploy{}, fmt.Errorf("read %s: %w", path, err)
	}
	var deploy Deploy
	if err := decodeStrictYAML(data, &deploy); err != nil {
		return Deploy{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := deploy.validate(); err != nil {
		return Deploy{}, err
	}
	return deploy, nil
}

// decodeStrictYAML decodes exactly one YAML document into dest and refuses unknown keys.
func decodeStrictYAML(data []byte, dest any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple documents")
		}
		return err
	}
	return nil
}

func (d Deploy) validate() error {
	for _, field := range []struct {
		name, value string
	}{
		{"bare_repo", d.BareRepo}, {"landing_repo", d.LandingRepo}, {"ledger_dsn_file", d.LedgerDSNFile},
		{"vm_host", d.VMHost}, {"ssh_config", d.SSHConfig}, {"vm_root", d.VMRoot}, {"graph_dir", d.GraphDir},
		{"state_dir", d.StateDir}, {"sessions_dir", d.SessionsDir}, {"score_work", d.ScoreWork},
		{"upstream.url", d.Upstream.URL}, {"upstream.branch", d.Upstream.Branch}, {"upstream.cache_host", d.Upstream.CacheHost},
		{"upstream.cache_dir", d.Upstream.CacheDir}, {"upstream.poll_seconds", d.Upstream.PollSeconds},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("missing %s", field.name)
		}
	}
	if d.LedgerRemotePort <= 0 || d.LedgerRemotePort > 65535 {
		return fmt.Errorf("ledger_remote_port must be a TCP port from 1 to 65535")
	}
	if d.LedgerLocalPort <= 0 || d.LedgerLocalPort > 65535 {
		return fmt.Errorf("ledger_local_port must be a TCP port from 1 to 65535")
	}
	for name, hook := range d.Hooks {
		if strings.TrimSpace(hook.Dir) == "" {
			return fmt.Errorf("hooks.%s.dir is required", name)
		}
		if strings.TrimSpace(hook.Command) == "" {
			return fmt.Errorf("hooks.%s.command is required", name)
		}
	}
	return nil
}
