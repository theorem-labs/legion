// Command owner is the owner protocol's command. Its one command so far is librarian-intake, the concept gate's
// librarian command: it queues each near match and recheck that the gate delivers as a work item for the librarian.
//
// Exit codes: 0 done, 2 usage, 3 unable to check, 4 protocol error, 75 the ledger cannot be reached (the caller
// retries).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const usage = "usage: owner [--json] <command> ...\n" +
	"commands: " + librarianIntakeUsage

const (
	exitUsage       = 2
	exitUnable      = 3
	exitProtocol    = 4
	exitUnreachable = 75 // EX_TEMPFAIL: the ledger could not be reached; the caller retries
)

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

// unableError marks an input the command needs to decide that it cannot read.
type unableError struct{ err error }

func (e *unableError) Error() string { return e.err.Error() }
func (e *unableError) Unwrap() error { return e.err }

// exitError carries the exit code that a command chose for err, which writeError keeps: librarian-intake's codes are
// its caller's contract.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }
func unable(err error) error {
	if err == nil {
		return nil
	}
	return &unableError{err}
}

// session holds what one command run has: its context, environment and error output, and, once opened, the ledger.
type session struct {
	ctx    context.Context
	getenv func(string) string
	stderr io.Writer
	store  *ledger.Store
}

func (s *session) close() {
	if s.store != nil {
		s.store.Close()
	}
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	jsonOutput := false
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "--help", "-h":
			fmt.Fprintln(stdout, usage)
			return 0
		case "--json":
			jsonOutput = true
			args = args[1:]
		default:
			return writeError(stderr, jsonOutput, usagef("unknown option %s", args[0]))
		}
	}
	if len(args) == 0 {
		return writeError(stderr, jsonOutput, usagef("missing command"))
	}
	command, err := parseCommand(args)
	if err != nil {
		return writeError(stderr, jsonOutput, err)
	}
	s := &session{ctx: ctx, getenv: getenv, stderr: stderr}
	defer s.close()
	value, err := command(s)
	if err != nil {
		return writeError(stderr, jsonOutput, err)
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(value); err != nil {
			return writeError(stderr, false, err)
		}
		return 0
	}
	fmt.Fprintln(stdout, text(value))
	return 0
}

// parseCommand checks a command's arguments before anything is read, so a usage error never needs a ledger.
func parseCommand(args []string) (func(*session) (any, error), error) {
	name, args := args[0], args[1:]
	switch name {
	case "librarian-intake":
		return parseLibrarianIntake(args)
	}
	return nil, usagef("unknown command %q", name)
}

func writeError(stderr io.Writer, jsonOutput bool, err error) int {
	code := exitProtocol
	var usageErr *usageError
	var unableErr *unableError
	var coded *exitError
	switch {
	case errors.As(err, &coded):
		code = coded.code
	case errors.As(err, &usageErr):
		code = exitUsage
	case errors.As(err, &unableErr):
		code = exitUnable
	}
	if jsonOutput {
		_ = json.NewEncoder(stderr).Encode(map[string]any{"error": err.Error(), "exit": code})
	} else {
		fmt.Fprintln(stderr, "owner:", err)
		if code == exitUsage {
			fmt.Fprintln(stderr, usage)
		}
	}
	return code
}
