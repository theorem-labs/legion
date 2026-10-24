package messagetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleReport() Report {
	start := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	return Report{
		Run: "message-test-20261023t180000z", Prefix: "notifications.lxspec-message-test.", Nodes: []string{fn, st},
		StartedAt: start, FinishedAt: start.Add(90 * time.Second),
		Steps:      []StepTiming{{Name: "setup", StartedAt: start, Seconds: 1.5}, {Name: "wake", StartedAt: start.Add(2 * time.Second), Seconds: 3}},
		Panes:      []PaneCheck{{Owner: st, Pane: "%3", StartSeconds: 0.8, Env: map[string]string{"LEGION_OWNER": st}}},
		Published:  []Publication{{Step: "arrival", Kind: "all", Topic: all, ID: "evt-all", Receivers: []string{fn, st}}},
		Deliveries: []Latency{{Owner: st, ID: "evt-all", Topic: all, Seconds: 0.25}},
		KillLog:    []Kill{{Item: needR1, Drawn: MomentAfterClaim, Observed: MomentAfterClaim, Pane: "%3", RelaunchSeconds: 1.2}},
		Restart:    &Restart{Item: needR3, StopSeconds: 0.1, StartSeconds: 0.4, ItemSeconds: 2},
		Revisions: Revisions{
			Owner:        Build{Path: "github.com/sjawhar/legion/orchestration", Revision: "0123456789abcdef0123456789abcdef01234567", Modified: "false", GoVersion: "go1.27"},
			Dependencies: []Module{{Path: "gopkg.in/yaml.v3", Version: "v3.0.1"}},
			Legion:       "legion v9 (fedcba9876543210)",
			Omp:          "omp 1.57.0",
			Tmux:         "tmux 3.4",
			Git:          "git version 2.43.0",
			Graph:        map[string]string{"graph.json": "sha256:aa", "owners.json": "sha256:bb", "topics.json": "sha256:cc"},
		},
	}
}

func TestReportPassesOnlyWithoutFailures(t *testing.T) {
	report := sampleReport()
	report.Finish(nil)
	if !report.Passed {
		t.Fatal("a report with no failures did not pass")
	}
	report.Finish([]Failure{{Kind: FailMissed, Subject: "evt-all on " + all + " for " + fn, Detail: "0 of these notices recorded"}})
	if report.Passed {
		t.Fatal("a report with a failure passed")
	}
}

func TestReportWritesJSONAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	report := sampleReport()
	report.Finish([]Failure{{Kind: FailCompletedTwice, Subject: needR1, Detail: "completed 2 times"}})
	if err := report.Write(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Passed || len(decoded.Failures) != 1 || decoded.Steps[1].Name != "wake" || decoded.Seconds != 90 {
		t.Fatalf("report.json = %+v", decoded)
	}
	markdown, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FAILED", "| setup | 1.5 |", "%3", "evt-all", "0.25", MomentAfterClaim, needR1, "completed 2 times", "message-test-20261023t180000z",
		"## Revisions", "0123456789abcdef0123456789abcdef01234567", "gopkg.in/yaml.v3 v3.0.1", "legion v9 (fedcba9876543210)", "omp 1.57.0", "tmux 3.4", "git version 2.43.0", "sha256:cc"} {
		if !strings.Contains(string(markdown), want) {
			t.Errorf("report.md does not contain %q:\n%s", want, markdown)
		}
	}
}
