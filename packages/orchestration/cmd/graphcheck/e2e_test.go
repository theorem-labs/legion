package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
)

// landBin and graphcheckBin are the real land and graphcheck commands, built once by TestMain.
var landBin, graphcheckBin string

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "graphcheck-e2e-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		landBin, graphcheckBin = filepath.Join(dir, "land"), filepath.Join(dir, "graphcheck")
		for bin, pkg := range map[string]string{landBin: "../land", graphcheckBin: "."} {
			if out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "go build %s: %v\n%s", pkg, err, out)
				return 1
			}
		}
		return m.Run()
	}())
}

func writeJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "land.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runBin runs a built command and returns its exit code, stdout and stderr.
func runBin(t *testing.T, bin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout.String(), stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stdout.String(), stderr.String()
	default:
		t.Fatalf("%s: %v", bin, err)
		return 0, "", ""
	}
}

func landConfig(t *testing.T, f *graphchecktest.Fixture, members []map[string]any) string {
	t.Helper()
	return writeJSON(t, map[string]any{
		"batchId": "e2e", "repository": map[string]any{"name": "fixture", "path": f.Repo, "git_dir": filepath.Join(f.Repo, ".git")},
		"base": f.Base, "baseBranch": "main", "statePath": filepath.Join(t.TempDir(), "state.json"), "members": members,
		"build": []string{"sh", "-c", "exit 99"}, "hasher": []string{"true"}, "toolchain": []string{"true"},
		"escalations": map[string]any{"owner": "steward"},
		"ledger":      map[string]any{"command": []string{"true"}, "status": []string{"true"}, "migrationState": []string{"true"}},
		"checks": []map[string]any{{"name": "names", "format": "graph",
			"command": []string{graphcheckBin, "names", "--config", f.ConfigPath}}},
	})
}

// One member privatises Lib.Core.helper and another adds a file: land check refuses only the first, before any
// build, naming the declaration and the use the build would fail on.
func TestLandCheckRefusesTheMemberThatRemovedAUsedName(t *testing.T) {
	f := graphchecktest.New(t)
	culprit := f.Branch(t, "culprit", map[string][2]string{"Lib/Core.lean": {"def helper", "private def helper"}})
	bystander := f.Branch(t, "bystander", map[string][2]string{"Lib/Extra.lean": {"", "def extra : Nat := 1\n"}})
	config := landConfig(t, f, []map[string]any{
		{"migration": "culprit", "generation": 1, "head": culprit},
		{"migration": "bystander", "generation": 1, "head": bystander},
	})
	code, out, errOut := runBin(t, landBin, "check", "--config", config)
	var got landing.CheckReport
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.Checks) != 1 {
		t.Fatalf("land check: exit %d, output %s, stderr %s, %v", code, out, errOut, err)
	}
	v := got.Checks[0]
	if v.Verdict != "fail" || len(v.Refused) == 0 {
		t.Fatalf("names verdict = %+v", v)
	}
	for _, r := range v.Refused {
		if r.Member != "culprit" || r.Declaration != "Lib.Core.helper" || r.Strength != 3 || r.Evidence == "" {
			t.Fatalf("refusal %+v, want culprit for Lib.Core.helper with strong evidence", r)
		}
	}
	if v.Seconds <= 0 {
		t.Fatalf("names verdict %+v, want the check's own wall time recorded", v)
	}
}

// A member changes Lib/Open.lean while another privatises the declaration it uses: the module no longer
// compiles, C1 refuses it through Lean's error, and the landing program attributes that error to the member
// that changed the declaring file, exactly as it attributes its build blame.
func TestC1AttributesLeansErrorToTheChangedMember(t *testing.T) {
	f := graphchecktest.New(t)
	culprit := f.Branch(t, "culprit", map[string][2]string{"Lib/Core.lean": {"def helper", "private def helper"}})
	user := f.Branch(t, "user", map[string][2]string{
		"Lib/Open.lean": {"open Lib.Core", "open Lib.Core\n\n-- Lib/Open.lean keeps its use of helper."},
	})
	config := landConfig(t, f, []map[string]any{
		{"migration": "culprit", "generation": 1, "head": culprit},
		{"migration": "user", "generation": 1, "head": user},
	})
	code, out, errOut := runBin(t, landBin, "check", "--config", config)
	var got landing.CheckReport
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.Checks) != 1 {
		t.Fatalf("land check: exit %d, output %s, stderr %s, %v", code, out, errOut, err)
	}
	v := got.Checks[0]
	if v.Verdict != "fail" || len(v.Refused) == 0 {
		t.Fatalf("names verdict = %+v", v)
	}
	sawC1 := false
	for _, r := range v.Refused {
		if r.Member != "culprit" {
			t.Fatalf("refusal %+v, want the member that made Lib.Core.helper private", r)
		}
		sawC1 = sawC1 || (strings.HasPrefix(r.Reason, "C1:") && r.Path == "Lib/Open.lean")
	}
	if !sawC1 {
		t.Fatalf("refusals %+v, want Lean's own error in Lib/Open.lean among them", v.Refused)
	}
}

// Six members each privatise Lib.Core.helper and add a file of their own, so Pick bounds the name (more than five
// strong rows) and the names check must be unable for the batch, with no member refused (D15).
func TestLandCheckWithBroadEvidenceIsUnable(t *testing.T) {
	f := graphchecktest.New(t)
	var members []map[string]any
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("m%d", i)
		head := f.Branch(t, id, map[string][2]string{
			"Lib/Core.lean":                    {"def helper", "private def helper"},
			fmt.Sprintf("Lib/Extra%d.lean", i): {"", fmt.Sprintf("def extra%d : Nat := %d\n", i, i)},
		})
		members = append(members, map[string]any{"migration": id, "generation": 1, "head": head})
	}
	code, out, errOut := runBin(t, landBin, "check", "--config", landConfig(t, f, members))
	var got struct{ Checks []landing.CheckVerdict }
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 1 || len(got.Checks) != 1 {
		t.Fatalf("land check: exit %d, output %s, stderr %s, %v", code, out, errOut, err)
	}
	if v := got.Checks[0]; v.Verdict != "unable" || len(v.Refused) != 0 || !strings.Contains(v.Reason, "too many strong matches") {
		t.Fatalf("names verdict = %+v", v)
	}
}
