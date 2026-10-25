// Command recall measures file-level recall of a declindex states result against a ground-truth file list.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type stating struct {
	Declaration string `json:"declaration"`
	At          string `json:"at"`
}

type witness struct {
	file        string
	declaration string
	at          string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("recall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statesPath := flags.String("states", "", "declindex states JSON output")
	concept := flags.String("concept", "", "concept to measure")
	truthPath := flags.String("truth", "", "newline-delimited ground-truth files")
	extrasPath := flags.String("extras", "", "TSV output for files outside the ground truth")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *statesPath == "" || *concept == "" || *truthPath == "" || *extrasPath == "" {
		fmt.Fprintln(stderr, "usage: recall --states states.json --concept C --truth truth.txt --extras extras.tsv")
		return 2
	}

	rows, err := readStates(*statesPath, *concept)
	if err != nil {
		fmt.Fprintf(stderr, "recall: %v\n", err)
		return 2
	}
	truth, err := readTruth(*truthPath)
	if err != nil {
		fmt.Fprintf(stderr, "recall: %v\n", err)
		return 2
	}
	if len(truth) == 0 {
		fmt.Fprintln(stderr, "recall: truth has no files")
		return 2
	}

	stateFiles := make(map[string]witness, len(rows))
	for _, row := range rows {
		if first, ok := stateFiles[row.file]; !ok || witnessLess(row, first) {
			stateFiles[row.file] = row
		}
	}
	matched := 0
	for file := range truth {
		if _, ok := stateFiles[file]; ok {
			matched++
		}
	}
	extras := make([]witness, 0, len(stateFiles))
	for file, row := range stateFiles {
		if _, ok := truth[file]; !ok {
			extras = append(extras, row)
		}
	}
	sort.Slice(extras, func(i, j int) bool { return witnessLess(extras[i], extras[j]) })
	if err := writeExtras(*extrasPath, extras); err != nil {
		fmt.Fprintf(stderr, "recall: write extras: %v\n", err)
		return 2
	}

	fmt.Fprintf(stdout, "concept\t%s\n", *concept)
	fmt.Fprintf(stdout, "truth_files\t%d\n", len(truth))
	fmt.Fprintf(stdout, "state_files\t%d\n", len(stateFiles))
	fmt.Fprintf(stdout, "matched_files\t%d\n", matched)
	fmt.Fprintf(stdout, "missing_files\t%d\n", len(truth)-matched)
	fmt.Fprintf(stdout, "extra_files\t%d\n", len(extras))
	fmt.Fprintf(stdout, "recall\t%.6f\n", float64(matched)/float64(len(truth)))
	return 0
}

func readStates(path, concept string) ([]witness, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read states: %w", err)
	}
	var states map[string][]stating
	if err := json.Unmarshal(body, &states); err != nil {
		return nil, fmt.Errorf("decode states: %w", err)
	}
	rows, ok := states[concept]
	if !ok {
		return nil, fmt.Errorf("states has no concept %q", concept)
	}
	out := make([]witness, 0, len(rows))
	for _, row := range rows {
		if row.Declaration == "" || row.At == "" {
			return nil, errors.New("each states row needs declaration and at")
		}
		file, ok := fileAt(row.At)
		if !ok {
			return nil, fmt.Errorf("states location %q is not path:line", row.At)
		}
		out = append(out, witness{file: file, declaration: row.Declaration, at: row.At})
	}
	return out, nil
}

func readTruth(path string) (map[string]struct{}, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	files := map[string]struct{}{}
	for _, line := range strings.Split(string(body), "\n") {
		file := strings.TrimSpace(line)
		if file == "" || strings.HasPrefix(file, "#") {
			continue
		}
		files[filepath.Clean(file)] = struct{}{}
	}
	return files, nil
}

func fileAt(at string) (string, bool) {
	i := strings.LastIndexByte(at, ':')
	if i <= 0 {
		return "", false
	}
	if _, err := strconv.Atoi(at[i+1:]); err != nil {
		return "", false
	}
	return filepath.Clean(at[:i]), true
}

func witnessLess(a, b witness) bool {
	if a.file != b.file {
		return a.file < b.file
	}
	if a.at != b.at {
		return a.at < b.at
	}
	return a.declaration < b.declaration
}

func writeExtras(path string, extras []witness) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, "file\tdeclaration\tat"); err != nil {
		return err
	}
	for _, row := range extras {
		if strings.ContainsAny(row.file+row.declaration+row.at, "\t\n\r") {
			return errors.New("state data contains a TSV control character")
		}
		if _, err := fmt.Fprintf(file, "%s\t%s\t%s\n", row.file, row.declaration, row.at); err != nil {
			return err
		}
	}
	return file.Close()
}
