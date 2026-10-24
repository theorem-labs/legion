// Command parity compares declindex users JSON with the reference uses tool's JSON for named declarations.
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

type answersDocument struct {
	Answers []answer `json:"answers"`
}

type answer struct {
	Declaration string `json:"declaration"`
	Direct      direct `json:"direct"`
	Reach       reach  `json:"reach"`
}

type direct struct {
	Uses         int                       `json:"uses"`
	Declarations int                       `json:"declarations"`
	Directories  int                       `json:"directories"`
	StmtUses     int                       `json:"stmt_uses"`
	ProofUses    int                       `json:"proof_uses"`
	ByDirectory  map[string]map[string]int `json:"by_directory"`
}

type reach struct {
	StatementsChanged    int `json:"statements_changed"`
	StatementDirectories int `json:"statement_directories"`
	ProofsToRedo         int `json:"proofs_to_redo"`
	ProofDirectories     int `json:"proof_directories"`
	AllDirectories       int `json:"all_directories"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("parity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	declindexPath := flags.String("declindex", "", "declindex users --json output")
	usesPath := flags.String("uses", "", "uses --json output")
	namesPath := flags.String("names", "", "newline-delimited full declaration names")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *declindexPath == "" || *usesPath == "" || *namesPath == "" {
		fmt.Fprintln(stderr, "usage: parity --declindex declindex-users.json --uses uses-users.json --names names.txt")
		return 2
	}
	names, err := readNames(*namesPath)
	if err != nil {
		fmt.Fprintf(stderr, "parity: %v\n", err)
		return 2
	}
	index, err := readAnswers(*declindexPath)
	if err != nil {
		fmt.Fprintf(stderr, "parity: read declindex output: %v\n", err)
		return 2
	}
	uses, err := readAnswers(*usesPath)
	if err != nil {
		fmt.Fprintf(stderr, "parity: read uses output: %v\n", err)
		return 2
	}

	different := false
	for _, name := range names {
		left, leftOK := index[name]
		right, rightOK := uses[name]
		if !leftOK || !rightOK {
			fmt.Fprintf(stdout, "%s\tanswer\tdeclindex=%s\tuses=%s\n", name, availability(leftOK), availability(rightOK))
			different = true
			continue
		}
		if compare(name, left, right, stdout) {
			different = true
		}
	}
	if different {
		return 1
	}
	fmt.Fprintf(stdout, "parity\t%d declarations match\n", len(names))
	return 0
}

func readNames(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(string(body), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		set[name] = struct{}{}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("names has no declarations")
	}
	return slices.Sorted(maps.Keys(set)), nil
}

func readAnswers(path string) (map[string]answer, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document answersDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	out := make(map[string]answer, len(document.Answers))
	for _, answer := range document.Answers {
		if answer.Declaration == "" {
			return nil, fmt.Errorf("an answer has no declaration")
		}
		if _, exists := out[answer.Declaration]; exists {
			return nil, fmt.Errorf("duplicate answer for %s", answer.Declaration)
		}
		out[answer.Declaration] = answer
	}
	return out, nil
}

func availability(present bool) string {
	if present {
		return "present"
	}
	return "missing"
}

func compare(name string, left, right answer, out io.Writer) bool {
	different := false
	check := func(field string, got, want int) {
		if got != want {
			fmt.Fprintf(out, "%s\t%s\tdeclindex=%d\tuses=%d\n", name, field, got, want)
			different = true
		}
	}
	check("direct.uses", left.Direct.Uses, right.Direct.Uses)
	check("direct.declarations", left.Direct.Declarations, right.Direct.Declarations)
	check("direct.directories", left.Direct.Directories, right.Direct.Directories)
	check("direct.stmt_uses", left.Direct.StmtUses, right.Direct.StmtUses)
	check("direct.proof_uses", left.Direct.ProofUses, right.Direct.ProofUses)
	for _, key := range directoryKeys(left.Direct.ByDirectory, right.Direct.ByDirectory) {
		check("direct.by_directory."+key.directory+"."+key.kind, left.Direct.ByDirectory[key.directory][key.kind], right.Direct.ByDirectory[key.directory][key.kind])
	}
	check("reach.statements_changed", left.Reach.StatementsChanged, right.Reach.StatementsChanged)
	check("reach.statement_directories", left.Reach.StatementDirectories, right.Reach.StatementDirectories)
	check("reach.proofs_to_redo", left.Reach.ProofsToRedo, right.Reach.ProofsToRedo)
	check("reach.proof_directories", left.Reach.ProofDirectories, right.Reach.ProofDirectories)
	check("reach.all_directories", left.Reach.AllDirectories, right.Reach.AllDirectories)
	return different
}

type directoryKey struct {
	directory string
	kind      string
}

func directoryKeys(left, right map[string]map[string]int) []directoryKey {
	set := map[directoryKey]struct{}{}
	for _, side := range []map[string]map[string]int{left, right} {
		for directory, kinds := range side {
			for kind := range kinds {
				set[directoryKey{directory, kind}] = struct{}{}
			}
		}
	}
	return slices.SortedFunc(maps.Keys(set), func(a, b directoryKey) int {
		return cmp.Or(strings.Compare(a.directory, b.directory), strings.Compare(a.kind, b.kind))
	})
}
