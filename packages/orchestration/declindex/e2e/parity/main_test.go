package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAcceptsMatchingAnswersForNamedDeclarations(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "declindex.json")
	uses := filepath.Join(dir, "uses.json")
	names := filepath.Join(dir, "names.txt")
	answer := `{"index":"abc","answers":[{"declaration":"Linux.C","direct":{"uses":2,"declarations":1,"directories":1,"stmt_uses":1,"proof_uses":1,"by_directory":{"Linux":{"Spec stmt":1,"Proof proof":1}}},"reach":{"statements_changed":3,"statement_directories":1,"proofs_to_redo":2,"proof_directories":1,"all_directories":1}}]}`
	for path, body := range map[string]string{index: answer, uses: answer, names: "Linux.C\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--declindex", index, "--uses", uses, "--names", names}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr.String())
	}
	if got, want := stdout.String(), "parity\t1 declarations match\n"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestRunPrintsEveryDirectDirectoryAndReachDifference(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "declindex.json")
	uses := filepath.Join(dir, "uses.json")
	names := filepath.Join(dir, "names.txt")
	indexAnswer := `{"answers":[{"declaration":"Linux.C","direct":{"uses":2,"declarations":1,"directories":1,"stmt_uses":1,"proof_uses":1,"by_directory":{"Linux":{"Spec stmt":2}}},"reach":{"statements_changed":3,"statement_directories":1,"proofs_to_redo":2,"proof_directories":1,"all_directories":1}}]}`
	usesAnswer := `{"answers":[{"declaration":"Linux.C","direct":{"uses":1,"declarations":1,"directories":1,"stmt_uses":1,"proof_uses":0,"by_directory":{"Linux":{"Spec stmt":1,"Proof proof":1}}},"reach":{"statements_changed":4,"statement_directories":1,"proofs_to_redo":2,"proof_directories":1,"all_directories":1}}]}`
	for path, body := range map[string]string{index: indexAnswer, uses: usesAnswer, names: "Linux.C\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--declindex", index, "--uses", uses, "--names", names}, &stdout, &stderr); code != 1 {
		t.Fatalf("run exited %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{
		"Linux.C\tdirect.uses\tdeclindex=2\tuses=1",
		"Linux.C\tdirect.proof_uses\tdeclindex=1\tuses=0",
		"Linux.C\tdirect.by_directory.Linux.Proof proof\tdeclindex=0\tuses=1",
		"Linux.C\tdirect.by_directory.Linux.Spec stmt\tdeclindex=2\tuses=1",
		"Linux.C\treach.statements_changed\tdeclindex=3\tuses=4",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout.String())
		}
	}
}
