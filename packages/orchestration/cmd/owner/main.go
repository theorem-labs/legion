// Command owner is the owner protocol's one command for owner sessions and people: it reports who the owner is
// and what it subscribes to, keeps its worktree, orders its queue, and runs the need, offer, accept and reply
// steps, each as one record commit, its follow-up items, its Envoy notices and the completion of the item it
// answers.
//
// Exit codes: 0 done, 1 refused, 2 usage, 3 unable to check, 4 protocol error.
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
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

const usage = "usage: owner [--json] [--deploy PATH] <command> ...\n" +
	"commands: whoami | topics | worktree | inbox | claim <item> |\n" +
	"  need <target> <file> [--outside-part | --answers <reply item>...] | offer <file> [--answers <need or reply item>...] |\n" +
	"  accept <item> | reply <item> <reason> <file> [--premise-owner <id>] |\n" +
	"  done <need or offer item> --superseded r<n> | returned <item>"

const (
	exitRefused  = 1
	exitUsage    = 2
	exitUnable   = 3
	exitProtocol = 4
)

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

type refusedError struct{ message string }

func (e *refusedError) Error() string { return e.message }

// unableError marks an input the command needs to decide (the graph, the rule, the score) that it cannot read.
type unableError struct{ err error }

func (e *unableError) Error() string { return e.err.Error() }
func (e *unableError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }
func refusef(format string, args ...any) error {
	return &refusedError{fmt.Sprintf(format, args...)}
}
func unable(err error) error {
	if err == nil {
		return nil
	}
	return &unableError{err}
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, ".", os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, cwd string, stdout, stderr io.Writer) int {
	jsonOutput := false
	deployPath := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "--help", "-h":
			fmt.Fprintln(stdout, usage)
			return 0
		case "--json":
			jsonOutput = true
			args = args[1:]
		case "--deploy":
			if len(args) < 2 {
				return writeError(stderr, jsonOutput, usagef("--deploy needs a path"))
			}
			deployPath, args = args[1], args[2:]
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
	s := &session{ctx: ctx, getenv: getenv, cwd: cwd, deployPath: deployPath}
	defer s.close()
	value, err := command(s)
	if err != nil {
		return writeError(stderr, jsonOutput, err)
	}
	// A refused notice does not fail a step: the runner journaled it, and the command names it.
	if result, ok := value.(stepResult); ok {
		for _, topic := range result.Undelivered {
			fmt.Fprintln(stderr, "notice not delivered:", topic)
		}
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
	case "whoami", "topics", "worktree", "inbox":
		if len(args) != 0 {
			return nil, usagef("%s takes no arguments", name)
		}
		return map[string]func(*session) (any, error){
			"whoami": (*session).whoami, "topics": (*session).topics, "worktree": (*session).worktreeCommand, "inbox": (*session).inbox,
		}[name], nil
	case "claim":
		if len(args) != 1 {
			return nil, usagef("claim <item>")
		}
		return func(s *session) (any, error) { return s.claim(args[0]) }, nil
	case "need":
		if len(args) < 2 {
			return nil, usagef("need <target> <file> [--outside-part | --answers <reply item>...]")
		}
		target, file, rest := args[0], args[1], args[2:]
		outside := len(rest) == 1 && rest[0] == "--outside-part"
		var answers []string
		switch {
		case len(rest) == 0 || outside:
		case len(rest) > 1 && rest[0] == "--answers":
			answers = rest[1:]
		default:
			return nil, usagef("need <target> <file> [--outside-part | --answers <reply item>...]")
		}
		return func(s *session) (any, error) { return s.need(target, file, outside, answers) }, nil
	case "offer":
		if len(args) == 0 || (len(args) > 1 && (args[1] != "--answers" || len(args) == 2)) {
			return nil, usagef("offer <file> [--answers <item>...]")
		}
		var answers []string
		if len(args) > 2 {
			answers = args[2:]
		}
		return func(s *session) (any, error) { return s.offer(args[0], answers) }, nil
	case "accept":
		if len(args) != 1 {
			return nil, usagef("accept <item>")
		}
		return func(s *session) (any, error) { return s.accept(args[0]) }, nil
	case "reply":
		premise := ""
		switch {
		case len(args) == 5 && args[3] == "--premise-owner" && args[4] != "":
			premise = args[4]
		case len(args) != 3:
			return nil, usagef("reply <item> <reason> <file> [--premise-owner <id>]")
		}
		return func(s *session) (any, error) { return s.reply(args[0], args[1], args[2], premise) }, nil
	case "done":
		if len(args) != 3 || args[1] != "--superseded" {
			return nil, usagef("done <need or offer item> --superseded r<n>")
		}
		ref, err := ownerpkg.ParseItemID(args[0])
		if err != nil || (ref.Type != "need" && ref.Type != "offer") {
			return nil, usagef("done takes a need or an offer item, whose newer revision supersedes it, not %q", args[0])
		}
		if _, err := ownerpkg.ParseRevision(args[2]); err != nil {
			return nil, usagef("done %s --superseded: %v", args[0], err)
		}
		return func(s *session) (any, error) { return s.done(args[0], ref, args[2]) }, nil
	case "returned":
		if len(args) != 1 {
			return nil, usagef("returned <item>")
		}
		return func(s *session) (any, error) { return s.returned(args[0]) }, nil
	}
	return nil, usagef("unknown command %q", name)
}

func writeError(stderr io.Writer, jsonOutput bool, err error) int {
	code := exitProtocol
	var usageErr *usageError
	var missing *ownerpkg.MissingEnvError
	var refused *refusedError
	var unableErr *unableError
	var refusal *ledger.Refusal
	var notFound *ledger.NotFoundError
	switch {
	case errors.As(err, &usageErr), errors.As(err, &missing):
		code = exitUsage
	case errors.As(err, &refused), errors.As(err, &refusal), errors.As(err, &notFound), errors.Is(err, ownerpkg.ErrIDOwner):
		code = exitRefused
	case errors.As(err, &unableErr), errors.Is(err, ownerpkg.ErrListenerConfig):
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

func isNotFound(err error) bool {
	var notFound *ledger.NotFoundError
	return errors.As(err, &notFound)
}
