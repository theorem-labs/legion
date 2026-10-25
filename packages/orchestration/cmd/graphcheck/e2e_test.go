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
	"github.com/sjawhar/legion/orchestration/internal/testdocker"
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

// landConfig is a land configuration whose only check is this graphcheck binary. Every check now runs in a
// container on the build VM, so the container section stands the host in for that VM (as cmd/land's own tests do)
// and gives the container read-only mounts of everything the check reads: the repository, the index and the Lean
// toolchain the fixture built with.
func landConfig(t *testing.T, f *graphchecktest.Fixture, members []map[string]any) string {
	t.Helper()
	user := testdocker.Require(t)
	root := t.TempDir()
	clone, runs, cache := filepath.Join(root, "clone.git"), filepath.Join(root, "runs"), filepath.Join(root, "lakecache")
	for _, dir := range []string{clone, runs, cache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []map[string]any{{"name": "lake-cache", "host_path": cache}}
	shared := map[string]any{}
	// The binary's own directory, never its parent: TestMain makes it directly in the temporary directory, so its
	// parent is /tmp itself, and a read-only /tmp would leave the check nowhere to compile.
	for i, dir := range []string{f.Repo, filepath.Dir(f.ConfigPath), filepath.Dir(graphcheckBin), f.Lean.Prefix} {
		name := "read-" + string(rune('a'+i))
		mounts = append(mounts, map[string]any{"name": name, "host_path": dir})
		shared[name] = "read-only"
	}
	build := map[string]any{"lake-cache": "read-write"}
	for name, mode := range shared {
		build[name] = mode
	}
	return writeJSON(t, map[string]any{
		"batchId": "e2e", "repository": map[string]any{"name": "fixture", "path": f.Repo, "git_dir": clone},
		"base": f.Base, "baseBranch": "main", "members": members,
		"build": []string{"sh", "-c", "exit 99"}, "hasher": []string{"true"}, "toolchain": []string{"true"},
		"escalations": map[string]any{"owner": "steward"},
		"container": map[string]any{
			"ssh": testdocker.SSH(t), "image": testdocker.Image, "user": user,
			"run_root": runs, "build_root": "lake-cache", "mounts": mounts,
			"build_mounts": build, "check_mounts": shared,
		},
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

// Lean's errors for a qualified name and for a type mismatch carry weak names too. Through the real land check,
// neither may turn into a line graphcheck cannot write, which would fail the whole batch: the qualified name is
// refused and attributed, and a mismatch that names nothing primary leaves the check unable for its own reason.
func TestLandCheckWithWeakNamesInLeansErrors(t *testing.T) {
	f := graphchecktest.New(t)
	culprit := f.Branch(t, "culprit", map[string][2]string{"Lib/Core.lean": {"def helper", "private def helper"}})
	user := f.Branch(t, "user", map[string][2]string{
		"Lib/Use.lean": {"namespace Lib.Use", "namespace Lib.Use\n\n-- Lib/Use.lean keeps its use of Lib.Core.helper."},
	})
	code, out, errOut := runBin(t, landBin, "check", "--config", landConfig(t, f, []map[string]any{
		{"migration": "culprit", "generation": 1, "head": culprit},
		{"migration": "user", "generation": 1, "head": user},
	}))
	var got landing.CheckReport
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.Checks) != 1 {
		t.Fatalf("qualified name: land check exit %d, output %s, stderr %s, %v", code, out, errOut, err)
	}
	if v := got.Checks[0]; v.Verdict != "fail" || len(v.Refused) == 0 || v.Refused[0].Member != "culprit" {
		t.Fatalf("qualified name: names verdict = %+v", v)
	}

	mismatch := f.Branch(t, "mismatch", map[string][2]string{
		"Lib/Open.lean": {"def viaOpen : Nat := helper 2", "def viaOpen : Nat := helper 2\n\ndef bad : Lib.Core.Box := Lib.Core.size"},
	})
	code, out, errOut = runBin(t, landBin, "check", "--config", landConfig(t, f, []map[string]any{
		{"migration": "mismatch", "generation": 1, "head": mismatch},
	}))
	got = landing.CheckReport{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Checks) != 1 {
		t.Fatalf("type mismatch: land check exit %d, output %s, stderr %s, %v", code, out, errOut, err)
	}
	if v := got.Checks[0]; strings.Contains(v.Reason, "weak") || strings.Contains(v.Reason, "is not unknown, added, changed or field") {
		t.Fatalf("type mismatch: names verdict %+v carries a weak line", v)
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
