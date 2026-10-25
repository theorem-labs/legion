package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing"
)

func checkConfiguration(t *testing.T, repo, base, state string, members []map[string]any, checks []map[string]any) string {
	t.Helper()
	return writeConfig(t, map[string]any{
		"batchId": "b", "repository": map[string]any{"name": "r", "path": repo, "git_dir": "/unused"}, "base": base,
		"baseBranch": "trunk", "statePath": state, "members": members,
		"build":  []string{"sh", "-c", "exit 99"},
		"hasher": []string{"true"}, "toolchain": []string{"true"},
		"ledger": map[string]any{"check": []string{"true"}, "land": []string{"true"}, "return": []string{"true"}, "status": []string{"true"}, "migrationState": []string{"true"}},
		"checks": checks,
	})
}

func TestLandCheckRunsChecksWithoutBuildingOrSaving(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0", "other": "+def extra := 1"})
	state := filepath.Join(t.TempDir(), "state.json")
	script := writeScript(t, `test "$LAND_BASE" = `+base+` || exit 9; printf 'refuse\tunknown\tAlpha.gone\tAlpha/Use.lean:2:12\tN1: Alpha/Core.lean no longer declares Alpha.gone\n'`)
	config := checkConfiguration(t, repo, base, state, []map[string]any{
		{"migration": "culprit", "generation": 1, "head": heads["culprit"]},
		{"migration": "other", "generation": 1, "head": heads["other"]},
	}, []map[string]any{{"name": "names", "command": []string{script}, "format": "graph"}})
	var out bytes.Buffer
	if err := run(context.Background(), []string{"check", "--config", config}, &out); err != nil {
		t.Fatalf("run() = %v, output %s", err, out.String())
	}
	var got landing.CheckReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output %s: %v", out.String(), err)
	}
	if got.Base != base || got.Tree == "" || !reflect.DeepEqual(got.Members, []string{"culprit", "other"}) || len(got.Checks) != 1 {
		t.Fatalf("report = %+v", got)
	}
	if v := got.Checks[0]; v.Verdict != "fail" || len(v.Refused) != 1 || v.Refused[0].Member != "culprit" || v.Refused[0].Evidence != "removed" {
		t.Fatalf("names verdict = %+v", v)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("land check wrote state: %v", err)
	}
}

// Every check runs, even after one refuses, and an unable check makes land
// check fail with exit 1 after it printed the report.
func TestLandCheckRunsEveryCheckAndReportsUnable(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0"})
	refuse := writeScript(t, `printf 'refuse\tunknown\tAlpha.gone\tAlpha/Use.lean:2:12\tN1: x\n'`)
	unable := writeScript(t, `echo 'index of another tree' >&2; exit 3`)
	config := checkConfiguration(t, repo, base, filepath.Join(t.TempDir(), "state.json"),
		[]map[string]any{{"migration": "culprit", "generation": 1, "head": heads["culprit"]}},
		[]map[string]any{{"name": "names", "command": []string{refuse}, "format": "graph"}, {"name": "statements", "command": []string{unable}, "format": "graph"}})
	var out bytes.Buffer
	err := run(context.Background(), []string{"check", "--config", config}, &out)
	if !errors.Is(err, errCheckUnable) {
		t.Fatalf("run() = %v, want errCheckUnable", err)
	}
	var got landing.CheckReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Checks) != 2 || got.Checks[0].Verdict != "fail" ||
		got.Checks[1].Verdict != "unable" || got.Checks[1].Reason != "index of another tree" {
		t.Fatalf("output %s, %v", out.String(), err)
	}
}

func TestLandCheckUsageIsExitTwo(t *testing.T) {
	var usage usageError
	for _, args := range [][]string{{"check"}, {"check", "--bogus"}, {"nothing"}, {}} {
		if err := run(context.Background(), args, &bytes.Buffer{}); !errors.As(err, &usage) {
			t.Errorf("run(%q) = %v, want a usage error", args, err)
		}
	}
}

func TestUnknownCheckFormatIsAConfigurationError(t *testing.T) {
	config := writeConfig(t, map[string]any{"checks": []map[string]any{{"name": "names", "command": []string{"true"}, "format": "xml"}}})
	if _, err := readConfig(config); err == nil || !strings.Contains(err.Error(), "checks[0].format") {
		t.Fatalf("readConfig() = %v, want an error naming checks[0].format", err)
	}
}
