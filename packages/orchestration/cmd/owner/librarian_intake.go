package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

const librarianIntakeUsage = "librarian-intake --id <id> --proposer <owner id> --proposal <file> --ledger-dsn-file <path> " +
	"--repository <name> [--git-dir <repo.git>] [--envoy-config <path>] [--topic-prefix <prefix>] [--deploy <path>]"

// intakeDeadline bounds one delivery: the gate kills its librarian command after 120 s (conceptgate/librarian.go:40).
const intakeDeadline = 110 * time.Second

type intakeFlags struct {
	id, proposer, proposal, dsnFile, repository, gitDir, envoyConfig, topicPrefix, deploy string
}

// intakeResult is what one delivery did. notices are the undelivered notices, which run prints.
type intakeResult struct {
	Item        string   `json:"item"`
	Kind        string   `json:"kind"`
	Added       bool     `json:"added"`
	Record      string   `json:"record,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	Undelivered []string `json:"undelivered,omitempty"`
	Journal     string   `json:"journal,omitempty"`
	notices     []ownerpkg.Notice
}

// parseLibrarianIntake reads the configured command's options and the three the gate appends (--id, --proposer,
// --proposal), each once, in any order. The gate's command gets no stdin, and this one reads none.
func parseLibrarianIntake(args []string) (func(*session) (any, error), error) {
	var flags intakeFlags
	values := map[string]*string{
		"--id": &flags.id, "--proposer": &flags.proposer, "--proposal": &flags.proposal, "--ledger-dsn-file": &flags.dsnFile,
		"--repository": &flags.repository, "--git-dir": &flags.gitDir, "--envoy-config": &flags.envoyConfig,
		"--topic-prefix": &flags.topicPrefix, "--deploy": &flags.deploy,
	}
	seen := map[string]bool{}
	for len(args) > 0 {
		name := args[0]
		value, known := values[name]
		switch {
		case !known:
			return nil, usagef("librarian-intake: unknown option %s", name)
		case seen[name]:
			return nil, usagef("librarian-intake: %s is given twice", name)
		case len(args) < 2 || args[1] == "":
			return nil, usagef("librarian-intake: %s needs a value", name)
		}
		seen[name], *value, args = true, args[1], args[2:]
	}
	for _, name := range []string{"--id", "--proposer", "--proposal", "--ledger-dsn-file", "--repository"} {
		if !seen[name] {
			return nil, usagef("librarian-intake: missing %s", name)
		}
	}
	kind := ownerpkg.GateIDKind(flags.id)
	switch {
	case kind == "":
		return nil, usagef("librarian-intake: --id %q is neither near-<16 hex digits> nor recheck-<key>-<key>", flags.id)
	case !ownerpkg.ValidOwnerID(flags.proposer):
		return nil, usagef("librarian-intake: --proposer %q is neither a node id nor a role name", flags.proposer)
	case flags.envoyConfig != "" && !filepath.IsAbs(flags.envoyConfig):
		return nil, usagef("librarian-intake: --envoy-config %q is not an absolute path", flags.envoyConfig)
	case flags.topicPrefix != "" && !ownerpkg.ValidTopicPrefix(flags.topicPrefix):
		return nil, usagef("librarian-intake: --topic-prefix %q is not a prefix under notifications. that ends in a dot", flags.topicPrefix)
	case kind == "recheck" && flags.gitDir == "":
		return nil, usagef("librarian-intake: a recheck delivery needs --git-dir, the librarian's repository that keeps its envelope")
	}
	return func(s *session) (any, error) { return s.librarianIntake(flags) }, nil
}

// librarianIntake queues the gate's delivery for the librarian (plan Task 1.10). It reads and checks everything before
// it writes: the envelope (exit 2), the journal (exit 3), the topic prefix (exit 2), the listener, and the ledger (exit
// 3, or 75 when it cannot be reached).
func (s *session) librarianIntake(flags intakeFlags) (any, error) {
	ctx, cancel := context.WithTimeout(s.ctx, intakeDeadline)
	defer cancel()
	text, err := os.ReadFile(flags.proposal)
	if err != nil {
		return nil, usagef("librarian-intake: read --proposal: %v", err)
	}
	envelope, err := ownerpkg.ParseGateEnvelope(text, flags.id, flags.proposer)
	if err != nil {
		return nil, usagef("librarian-intake: --proposal %s: %v", flags.proposal, err)
	}
	journal, err := s.intakeJournal(flags.deploy)
	if err != nil {
		return nil, err
	}
	intake := ownerpkg.LibrarianIntake{Repo: flags.repository, Journal: journal}
	if flags.gitDir != "" {
		intake.Inbox = &ownerpkg.Repo{Path: flags.gitDir}
	}
	prefix := flags.topicPrefix
	if prefix == "" {
		if intake.Inbox == nil {
			return nil, usagef("librarian-intake: no topic prefix: give --topic-prefix, or --git-dir whose main has %s", ownerpkg.ConfigPath)
		}
		config, err := ownerpkg.LoadConfig(intake.Inbox)
		if err != nil {
			return nil, usagef("librarian-intake: no topic prefix: --topic-prefix is not given, and %v", err)
		}
		prefix = config.TopicPrefix
	}
	if intake.Routing, err = ownerpkg.NewRouting(prefix, nil); err != nil {
		return nil, usagef("librarian-intake: --topic-prefix: %v", err)
	}
	if url, token, err := ownerpkg.ResolveListener(s.listenerEnv(flags.envoyConfig)); err != nil {
		intake.NoListener = fmt.Errorf("no Envoy listener: %w", err)
	} else {
		intake.Envoy = ownerpkg.NewHTTPPublisher(url, token)
	}

	ledgerCtx, cancelLedger := context.WithTimeout(ctx, ownerpkg.IntakeLedgerTimeout)
	defer cancelLedger()
	store, err := ownerpkg.OpenLedgerFile(ledgerCtx, "--ledger-dsn-file", flags.dsnFile)
	if err != nil {
		if ownerpkg.LedgerUnreachable(err) {
			return nil, &exitError{exitUnreachable, err}
		}
		return nil, &exitError{exitUnable, err}
	}
	s.store, intake.Ledger = store, store

	outcome, err := intake.Deliver(ctx, envelope)
	if err != nil {
		return nil, intakeError(err)
	}
	result := intakeResult{Item: outcome.Item, Kind: outcome.Kind, Added: outcome.Added, Record: outcome.Record, Commit: outcome.Commit,
		notices: outcome.Undelivered}
	for _, notice := range outcome.Undelivered {
		result.Undelivered = append(result.Undelivered, notice.Topic)
	}
	if len(result.Undelivered) > 0 {
		result.Journal = journal
	}
	return result, nil
}

// intakeError gives a failed delivery its exit code: 75 when the ledger could not be reached, so that the caller
// retries; 3 when the ledger cannot check it (no such repository, or the server refused a query, as a missing schema
// makes it); 4 otherwise, an item of another owner among them.
func intakeError(err error) error {
	var notFound *ledger.NotFoundError
	var server *pgconn.PgError
	switch {
	case ownerpkg.LedgerUnreachable(err):
		return &exitError{exitUnreachable, err}
	case errors.As(err, &notFound) && notFound.What == "repository", errors.As(err, &server):
		return &exitError{exitUnable, err}
	}
	return &exitError{exitProtocol, err}
}

// intakeJournal is the host's journal of undelivered notices, resolved before anything is written: the state_dir's
// undelivered.jsonl of the deployment file (deploy, else owner's --deploy, else ~/.config/owner/deploy.yaml) when there
// is one, else <user cache directory>/owner/undelivered/undelivered.jsonl. A named deployment file that is missing,
// or any deployment file that does not load, is exit 3.
func (s *session) intakeJournal(deploy string) (string, error) {
	path := deploy
	if path == "" {
		path = s.deployPath
	}
	named := path != ""
	if home := strings.TrimSpace(s.getenv("HOME")); !named && home != "" {
		path = filepath.Join(home, ".config", "owner", "deploy.yaml")
	}
	if path != "" {
		deployment, err := ownerpkg.LoadDeploy(path)
		switch {
		case err == nil && filepath.IsAbs(deployment.StateDir):
			return filepath.Join(deployment.StateDir, "undelivered.jsonl"), nil
		case err == nil:
			return "", unable(fmt.Errorf("%s: state_dir %q is not an absolute path", path, deployment.StateDir))
		case named || !errors.Is(err, os.ErrNotExist):
			return "", unable(fmt.Errorf("the deployment file: %w", err))
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", unable(fmt.Errorf("no deployment file and no user cache directory for the notice journal: %w", err))
	}
	return filepath.Join(cache, "owner", "undelivered", "undelivered.jsonl"), nil
}

// listenerEnv is the command's environment, with --envoy-config as ENVOY_CONFIG_FILE when it is given.
func (s *session) listenerEnv(envoyConfig string) func(string) string {
	if envoyConfig == "" {
		return s.getenv
	}
	return func(name string) string {
		if name == "ENVOY_CONFIG_FILE" {
			return envoyConfig
		}
		return s.getenv(name)
	}
}
