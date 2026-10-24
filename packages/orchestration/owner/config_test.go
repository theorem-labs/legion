package owner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadDeployReadsEveryConfiguredField(t *testing.T) {
	deploy, err := LoadDeploy("testdata/deploy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := Deploy{
		BareRepo: "/srv/owners/repo.git", LandingRepo: "/srv/owners/landing", LedgerDSNFile: "/srv/owners/ledger-dsn",
		VMHost: "build.example.test", SSHConfig: "/srv/owners/ssh_config", VMRoot: "/srv/vm/owners",
		GraphDir: "/srv/graph/out", StateDir: "/srv/owners/state", SessionsDir: "/srv/owners/sessions",
		ScoreWork: "/srv/owners/score", LedgerRemotePort: 15432, LedgerLocalPort: 5432,
		Hooks: map[string]Hook{"score": {
			Dir: "/srv/score", Command: "/usr/local/bin/score-input", Parts: []string{"slice"},
		}},
		Upstream: UpstreamConfig{
			URL: "https://example.test/upstream.git", Branch: "main", CacheHost: "cache.example.test",
			CacheDir: "/var/cache/owners", PollSeconds: "300",
		},
	}
	if !reflect.DeepEqual(deploy, want) {
		t.Fatalf("LoadDeploy = %+v, want %+v", deploy, want)
	}
}

func TestLoadDeployRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.yaml")
	if err := os.WriteFile(path, []byte("bare_repo: /repo\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDeploy(path)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("LoadDeploy unknown key error = %v", err)
	}
}

func TestLoadDeployRejectsInvalidRequiredValues(t *testing.T) {
	fixture, err := os.ReadFile("testdata/deploy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, before, after, field string
	}{
		{"missing bare repository", "bare_repo: /srv/owners/repo.git", "bare_repo: ", "bare_repo"},
		{"missing landing repository", "landing_repo: /srv/owners/landing", "landing_repo: ", "landing_repo"},
		{"missing ledger DSN file", "ledger_dsn_file: /srv/owners/ledger-dsn", "ledger_dsn_file: ", "ledger_dsn_file"},
		{"missing VM host", "vm_host: build.example.test", "vm_host: ", "vm_host"},
		{"missing SSH config", "ssh_config: /srv/owners/ssh_config", "ssh_config: ", "ssh_config"},
		{"missing VM root", "vm_root: /srv/vm/owners", "vm_root: ", "vm_root"},
		{"missing graph directory", "graph_dir: /srv/graph/out", "graph_dir: ", "graph_dir"},
		{"missing state directory", "state_dir: /srv/owners/state", "state_dir: ", "state_dir"},
		{"missing sessions directory", "sessions_dir: /srv/owners/sessions", "sessions_dir: ", "sessions_dir"},
		{"missing score work directory", "score_work: /srv/owners/score", "score_work: ", "score_work"},
		{"missing upstream URL", "url: https://example.test/upstream.git", "url: ", "upstream.url"},
		{"missing upstream branch", "branch: main", "branch: ", "upstream.branch"},
		{"missing upstream cache host", "cache_host: cache.example.test", "cache_host: ", "upstream.cache_host"},
		{"missing upstream cache directory", "cache_dir: /var/cache/owners", "cache_dir: ", "upstream.cache_dir"},
		{"missing upstream poll time", "poll_seconds: \"300\"", "poll_seconds: ", "upstream.poll_seconds"},
		{"invalid remote port", "ledger_remote_port: 15432", "ledger_remote_port: 65536", "ledger_remote_port"},
		{"invalid local port", "ledger_local_port: 5432", "ledger_local_port: 0", "ledger_local_port"},
		{"invalid score hook directory", "dir: /srv/score", "dir: ", "hooks.score.dir"},
		{"invalid score hook", "command: /usr/local/bin/score-input", "command: ", "hooks.score.command"},
		{"unknown upstream field", "poll_seconds: \"300\"", "poll_seconds: \"300\"\n  unexpected: true", "unexpected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "deploy.yaml")
			data := []byte(strings.Replace(string(fixture), tc.before, tc.after, 1))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDeploy(path)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("LoadDeploy error = %v, want %q", err, tc.field)
			}
		})
	}
}

func TestLoadDeployRejectsAdditionalDocument(t *testing.T) {
	fixture, err := os.ReadFile("testdata/deploy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deploy.yaml")
	if err := os.WriteFile(path, append(fixture, []byte("\n---\nbare_repo: /other\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadDeploy(path)
	if err == nil || !strings.Contains(err.Error(), "multiple documents") {
		t.Fatalf("LoadDeploy additional document error = %v", err)
	}
}

func TestDecodeStrictYAMLReadsOwnerConfiguration(t *testing.T) {
	data, err := os.ReadFile("testdata/owner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config Config
	if err := decodeStrictYAML(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Repo != "example-repo" || config.TopicPrefix != "notifications.example." ||
		config.Paths.NodeModuleTemplate != "Linux/Skeleton/{key}/Node.lean" ||
		!reflect.DeepEqual(config.Build.GateTargets, []string{"Linux", "Firmware", "Combined"}) {
		t.Fatalf("owner configuration = %+v", config)
	}
}

func TestLoadConfigReadsOwnerConfigurationAtMain(t *testing.T) {
	data, err := os.ReadFile("testdata/owner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := newTestRepo(t)
	main := testWorktree(t, repo, "main")
	if _, err := main.CommitFile(context.Background(), "skeleton/owner.yaml", data, "skeleton: owner configuration", nil); err != nil {
		t.Fatal(err)
	}
	branch := testWorktree(t, repo, "owner/other")
	if _, err := branch.CommitFile(context.Background(), "skeleton/owner.yaml", []byte("repo: other\n"), "other branch", nil); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(&repo)
	if err != nil {
		t.Fatal(err)
	}
	if config.Repo != "example-repo" || config.ActiveFile != "skeleton/active/ns-match.yaml" ||
		config.Paths.EdgeRecordTemplate != "skeleton/nodes/{key_source}/edges/E{hex16_target}.yaml" {
		t.Fatalf("LoadConfig = %+v", config)
	}
}

func TestLoadConfigNamesMissingKey(t *testing.T) {
	data, err := os.ReadFile("testdata/owner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	repo := newTestRepo(t)
	main := testWorktree(t, repo, "main")
	without := strings.Replace(string(data), "rule: skeleton/RULE.yaml\n", "", 1)
	if _, err := main.CommitFile(context.Background(), "skeleton/owner.yaml", []byte(without), "skeleton: owner configuration", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(&repo); err == nil || !strings.Contains(err.Error(), "rule") {
		t.Fatalf("LoadConfig missing rule error = %v", err)
	}
}

// Owners publish under the prefix their configuration names, any prefix under notifications. that ends in a dot; a
// test run, such as the message test, keeps its traffic apart under a prefix of its own so that no production owner
// receives it.
func TestValidTopicPrefixIsAnyPrefixUnderNotifications(t *testing.T) {
	for prefix, want := range map[string]bool{
		"notifications.example.":              true,
		"notifications.example.extra.":        true,
		"notifications.example-message-test.": true,
		"notifications.other.":                true,
		"notifications.example":               false,
		"notifications.example-message-test":  false,
		"notifications.":                      false,
		"notifications..":                     false,
		"notifications.example..":             false,
		"notifications.exa mple.":             false,
		"example.":                            false,
		"":                                    false,
	} {
		if got := ValidTopicPrefix(prefix); got != want {
			t.Errorf("ValidTopicPrefix(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func TestLoadTopicsAcceptsATestRunsPrefixUnderNotifications(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topics.json")
	document := `{"format":"cgraph/topics/v1","part":"slice","prefix":"notifications.example-message-test.","owners":{}}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTopics(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(document, "notifications.example-message-test.", "example.", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTopics(path); err == nil {
		t.Fatal("LoadTopics accepted a prefix outside notifications.")
	}
}
