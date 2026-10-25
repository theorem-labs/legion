// Command graphcheck runs one group of graph rules on a landing candidate before its build and prints each hit
// as a graph finding line (packages/orchestration/graphcheck/README.md).
//
//	graphcheck <names|statements|links|producers> --config FILE
//
// It reads LAND_TREE (the candidate tree) and LAND_BASE (the batch's base commit). It exits 0 when it decided,
// 2 on a usage or configuration error, and 3 when it cannot decide (one stderr line says why).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/orchestration/declindex"
	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/landing"
)

const usage = "usage: graphcheck <names|statements|links|producers> --config FILE"

type rule func(context.Context, *graphcheck.Candidate, *declindex.Store, graphcheck.Config) ([]landing.Finding, error)

var rules = map[string]rule{
	"names":      graphcheck.Names,
	"statements": graphcheck.Statements,
	"links":      graphcheck.Links,
	"producers":  graphcheck.Producers,
}

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

// run is the command: args without the program name, env as os.Environ gives it.
func run(args, env []string, stdout, stderr io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintln(stderr, strings.ReplaceAll(fmt.Sprintf(format, a...), "\n", " "))
		return code
	}
	if len(args) == 0 {
		return fail(2, "%s", usage)
	}
	check, ok := rules[args[0]]
	if !ok {
		return fail(2, "unknown rule group %q; %s", args[0], usage)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "graphcheck.yaml")
	if err := flags.Parse(args[1:]); err != nil || *configPath == "" || flags.NArg() != 0 {
		return fail(2, "%s", usage)
	}
	cfg, err := graphcheck.LoadConfig(*configPath)
	if err != nil {
		return fail(2, "%v", err)
	}
	ctx := context.Background()
	c, idx, err := graphcheck.Open(ctx, cfg, lookup(env, "LAND_BASE"), lookup(env, "LAND_TREE"))
	if err != nil {
		return failure(fail, err)
	}
	defer idx.Close()
	defer c.Close()
	findings, err := check(ctx, c, idx, cfg)
	if err != nil {
		return failure(fail, err)
	}
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		line, err := landing.FormatGraphFinding(f)
		if err != nil {
			return fail(1, "finding %+v: %v", f, err)
		}
		lines = append(lines, line)
	}
	w := bufio.NewWriter(stdout)
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	if err := w.Flush(); err != nil {
		return fail(1, "%v", err)
	}
	return 0
}

// failure exits 3 for a check that cannot decide and 1 for any other error.
func failure(fail func(int, string, ...any) int, err error) int {
	var unable *graphcheck.UnableError
	if errors.As(err, &unable) {
		return fail(3, "unable to check: %s", unable.Reason)
	}
	return fail(1, "%v", err)
}

func lookup(env []string, key string) string {
	value := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value = v
		}
	}
	return value
}
