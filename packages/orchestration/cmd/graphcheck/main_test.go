package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
)

type result struct {
	code           int
	stdout, stderr string
}

func invoke(t *testing.T, env []string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, env, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func parseLines(t *testing.T, stdout string) []landing.Finding {
	t.Helper()
	var out []landing.Finding
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		f, err := landing.ParseGraphFinding(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, f)
	}
	return out
}

func TestCommandPrintsFindingLinesAndExitCodes(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def helper", "private def helper")
	r := invoke(t, []string{"LAND_BASE=" + f.Base, "LAND_TREE=" + tree}, "names", "--config", f.ConfigPath)
	if r.code != 0 || r.stderr != "" || r.stdout == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	parseLines(t, r.stdout)
	if r := invoke(t, []string{"LAND_BASE=" + f.Base}, "names", "--config", f.ConfigPath); r.code != 3 || r.stdout != "" || r.stderr == "" {
		t.Fatalf("without LAND_TREE: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := invoke(t, nil, "nonsense"); r.code != 2 {
		t.Fatalf("unknown rule: exit %d", r.code)
	}
}

// The lines are sorted by path, line, column and declaration, whatever order the rules found them in.
func TestCommandSortsItsLines(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean": {"def helper", "private def helper"},
		"Lib/Make.lean": {"def one", "def zero : Lib.Core.Box := { val := 2 }\n\ndef one"},
	})
	r := invoke(t, []string{"LAND_BASE=" + f.Base, "LAND_TREE=" + tree}, "names", "--config", f.ConfigPath)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	hits := parseLines(t, r.stdout)
	if len(hits) < 2 {
		t.Fatalf("findings %v, want at least two", hits)
	}
	sorted := sort.SliceIsSorted(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return a.Declaration < b.Declaration
	})
	if !sorted {
		t.Fatalf("lines not sorted: %s", r.stdout)
	}
}

func TestCommandIsUnableForAnIndexOfAnotherTree(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Core.lean", "def size", "def size2")
	r := invoke(t, []string{"LAND_BASE=" + f.HeadOf(t, tree), "LAND_TREE=" + tree}, "names", "--config", f.ConfigPath)
	if r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "tree") {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestCommandUsageAndConfigurationErrorsAreExitTwo(t *testing.T) {
	f := graphchecktest.New(t)
	env := []string{"LAND_BASE=" + f.Base, "LAND_TREE=" + f.BaseTree(t)}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("index: x\nnope: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"names"}, {"names", "--config"}, {"names", "--config", f.ConfigPath, "extra"}, {"names", "--config", bad}, {"names", "--config", filepath.Join(t.TempDir(), "missing.yaml")}} {
		if r := invoke(t, env, args...); r.code != 2 || r.stdout != "" || r.stderr == "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
}

// Every rule group runs on the fixture and prints lines that land reads.
func TestEveryRuleGroupPrintsParseableLines(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Core.lean":        {"def size : Nat := 3", "def size : Nat := 4"},
		"Lib/Copy.lean":        {"", "namespace Lib.Copy\n\nstructure PortCopy where\n  id : Nat\n\nend Lib.Copy\n"},
		"Lib/Drv/LinkDrv.lean": {"", "import Lib.Spec.LinkSpec\nimport Lib.Iface\n\nopen Lib.Spec Lib.Iface\n\ntheorem keeps (h : SIZE_OK) (p : Port) : True := trivial\n"},
	})
	env := []string{"LAND_BASE=" + f.Base, "LAND_TREE=" + tree}
	for _, rule := range []string{"names", "statements", "links", "producers"} {
		r := invoke(t, env, rule, "--config", f.ConfigPath)
		if r.code != 0 || r.stderr != "" {
			t.Fatalf("%s: exit %d, stderr %q", rule, r.code, r.stderr)
		}
		hits := parseLines(t, r.stdout)
		if rule != "names" && len(hits) == 0 {
			t.Fatalf("%s printed no finding", rule)
		}
	}
}
