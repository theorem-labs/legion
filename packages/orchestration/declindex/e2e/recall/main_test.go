package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCountsTruthAndWritesFirstExtraWitness(t *testing.T) {
	dir := t.TempDir()
	states := filepath.Join(dir, "states.json")
	truth := filepath.Join(dir, "truth.txt")
	extras := filepath.Join(dir, "extras.tsv")
	if err := os.WriteFile(states, []byte(`{"C":[{"declaration":"First","at":"Linux/Extra.lean:3"},{"declaration":"Second","at":"Linux/Extra.lean:9"},{"declaration":"InTruth","at":"Linux/Truth.lean:7"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truth, []byte("Linux/Truth.lean\nLinux/Missing.lean\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--states", states, "--concept", "C", "--truth", truth, "--extras", extras}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr.String())
	}
	for _, want := range []string{"truth_files\t2", "state_files\t2", "matched_files\t1", "missing_files\t1", "extra_files\t1", "recall\t0.500000"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout.String())
		}
	}
	got, err := os.ReadFile(extras)
	if err != nil {
		t.Fatal(err)
	}
	want := "file\tdeclaration\tat\nLinux/Extra.lean\tFirst\tLinux/Extra.lean:3\n"
	if string(got) != want {
		t.Fatalf("extras:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunRejectsAnEmptyTruthSet(t *testing.T) {
	dir := t.TempDir()
	states := filepath.Join(dir, "states.json")
	truth := filepath.Join(dir, "truth.txt")
	extras := filepath.Join(dir, "extras.tsv")
	if err := os.WriteFile(states, []byte(`{"C":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truth, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--states", states, "--concept", "C", "--truth", truth, "--extras", extras}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "truth") {
		t.Fatalf("run exited %d, stderr %q", code, stderr.String())
	}
}
