package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/sjawhar/legion/orchestration/landing"
)

// errCheckUnable makes land exit 1 after land check printed its report: a
// check could not decide.
var errCheckUnable = fmt.Errorf("a check was unable")

// runCheck builds the configuration's candidate and runs every configured
// check on it. It starts no build, returns no member and writes no state. It
// prints {base, tree, members, checks}, and returns errCheckUnable when a
// check was unable.
func runCheck(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to the repository configuration")
	if err := flags.Parse(args); err != nil {
		return usageError{err}
	}
	if *configPath == "" || flags.NArg() != 0 {
		return usageError{fmt.Errorf("usage: land check --config FILE")}
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.Repository.Path == "" || cfg.Base == "" {
		return fmt.Errorf("repository.path and base are required")
	}
	runner := landing.Runner{Git: landing.RepositoryGit{Path: cfg.Repository.Path}, Checks: cfg.checkers()}
	report, err := runner.CheckCandidate(ctx, landing.Input{ID: cfg.BatchID, Repository: cfg.Repository.Name, Base: cfg.Base, BaseBranch: cfg.BaseBranch, Members: cfg.Members})
	if err != nil {
		return err
	}
	if err := writeJSON(output, report); err != nil {
		return err
	}
	for _, verdict := range report.Checks {
		if verdict.Verdict == "unable" {
			return errCheckUnable
		}
	}
	return nil
}
