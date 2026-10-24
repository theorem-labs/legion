package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

const librarianIntakeUsage = "librarian-intake --id <id> --proposer <owner id> --proposal <file> --ledger-dsn-file <path> " +
	"--repository <name> [--git-dir <repo.git>] [--envoy-config <path>] [--topic-prefix <prefix>] [--deploy <path>]"

// intakeDeadline bounds one delivery: the gate kills its librarian command after 120 s (conceptgate/librarian.go:41).
const intakeDeadline = 110 * time.Second

type intakeFlags struct {
	id, proposer, proposal, dsnFile, repository, gitDir, envoyConfig, topicPrefix, deploy string
}

// intakeResult is what one delivery did.
type intakeResult struct {
	Item        string   `json:"item"`
	Kind        string   `json:"kind"`
	Added       bool     `json:"added"`
	Record      string   `json:"record,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	Undelivered []string `json:"undelivered,omitempty"`
	Journal     string   `json:"journal,omitempty"`
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
// it writes: the envelope (exit 2), the journal and the deployment (exit 3), --git-dir (exit 3) and its configuration
// (exit 2 when it has none, 3 when it does not load), the topic prefix (exit 2), an --envoy-config (exit 3), the
// listener, and the ledger (exit 3, or 75 when it cannot be reached).
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
	journal, bareRepo, err := s.intakeDeployment(flags.deploy)
	if err != nil {
		return nil, err
	}
	intake := ownerpkg.LibrarianIntake{Repo: flags.repository, Journal: journal}
	if flags.gitDir != "" {
		intake.Inbox = &ownerpkg.Repo{Path: flags.gitDir}
		if err := checkBareRepo(ctx, flags.gitDir); err != nil {
			return nil, unable(err)
		}
		if bareRepo != "" && !samePath(bareRepo, flags.gitDir) {
			return nil, usagef("librarian-intake: --git-dir %s is not the deployment's bare_repo %s, which owner inbox reads", flags.gitDir, bareRepo)
		}
	}
	prefix := flags.topicPrefix
	if prefix == "" {
		if prefix, err = configuredPrefix(ctx, intake.Inbox, flags.repository); err != nil {
			return nil, err
		}
	}
	if intake.Routing, err = ownerpkg.NewRouting(prefix, nil); err != nil {
		return nil, usagef("librarian-intake: --topic-prefix: %v", err)
	}
	if flags.envoyConfig != "" {
		if err := ownerpkg.CheckEnvoyConfig(flags.envoyConfig); err != nil {
			return nil, unable(fmt.Errorf("--envoy-config: %w", err))
		}
	}
	if url, token, err := ownerpkg.ResolveListener(ctx, s.listenerEnv(flags.envoyConfig)); err != nil {
		intake.Envoy = ownerpkg.NoListener{Reason: err}
	} else {
		intake.Envoy = ownerpkg.NewHTTPPublisher(url, token)
	}

	ledgerCtx, cancelLedger := context.WithTimeout(ctx, ownerpkg.IntakeLedgerTimeout)
	defer cancelLedger()
	store, err := ownerpkg.OpenLedgerFile(ledgerCtx, "--ledger-dsn-file", flags.dsnFile)
	if err != nil {
		return nil, intakeError(err)
	}
	s.store, intake.Ledger = store, store

	outcome, err := intake.Deliver(ctx, envelope)
	if err != nil && ctx.Err() != nil {
		// The command's own deadline ended the delivery: a slow ledger or listener, which a retry may find well.
		err = &exitError{exitUnreachable, err}
	}
	return deliveryResult(s.stderr, outcome, err, journal)
}

// deliveryResult is the command's result for outcome. It prints each undelivered notice whole, also for a failed
// delivery, since the journal may have failed to keep it; a failed delivery gets its exit code (intakeError).
func deliveryResult(stderr io.Writer, outcome ownerpkg.IntakeOutcome, err error, journal string) (any, error) {
	printUndelivered(stderr, outcome.Undelivered, journal)
	if err != nil {
		return nil, intakeError(err)
	}
	result := intakeResult{Item: outcome.Item, Kind: outcome.Kind, Added: outcome.Added, Record: outcome.Record, Commit: outcome.Commit}
	for _, notice := range outcome.Undelivered {
		result.Undelivered = append(result.Undelivered, notice.Topic)
	}
	if len(result.Undelivered) > 0 {
		result.Journal = journal
	}
	return result, nil
}

// printUndelivered prints each whole notice that no listener took, with the journal meant to keep it.
func printUndelivered(stderr io.Writer, notices []ownerpkg.Notice, journal string) {
	for _, notice := range notices {
		data, _ := json.Marshal(map[string]any{"message": notice.Message, "payload": notice.Payload, "key": notice.Key})
		fmt.Fprintf(stderr, "notice not delivered: %s %s (journal %s)\n", notice.Topic, data, journal)
	}
}

// intakeError gives a failed delivery its exit code: an exit code it already has; 75 when the ledger could not be
// reached, so that the caller retries; 4 for a protocol error (an item of another owner, a recheck record of another
// proposer); 3 otherwise: the command could not check the delivery (no such repository, a query the server refused, a
// git or journal write that failed).
func intakeError(err error) error {
	var coded *exitError
	switch {
	case errors.As(err, &coded):
		return err
	case ownerpkg.LedgerUnreachable(err):
		return &exitError{exitUnreachable, err}
	case errors.Is(err, ownerpkg.ErrIDOwner), errors.Is(err, ownerpkg.ErrRecordProposer):
		return &exitError{exitProtocol, err}
	}
	return &exitError{exitUnable, err}
}

// checkBareRepo refuses a --git-dir that is not a bare repository.
func checkBareRepo(ctx context.Context, dir string) error {
	out, err := exec.CommandContext(ctx, "git", "--git-dir="+dir, "rev-parse", "--is-bare-repository").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("--git-dir %s is not a bare repository (%v)", dir, err)
	}
	return nil
}

// configuredPrefix is the topic_prefix of skeleton/owner.yaml at main of the librarian's repository, which must name
// the ledger repository: no repository or no file is exit 2 (ruling R2), a file that does not load exit 3.
func configuredPrefix(ctx context.Context, inbox *ownerpkg.Repo, repository string) (string, error) {
	if inbox == nil {
		return "", usagef("librarian-intake: no topic prefix: give --topic-prefix, or --git-dir whose main has %s", ownerpkg.ConfigPath)
	}
	found, err := inbox.HasFile(ctx, "main", ownerpkg.ConfigPath)
	if err != nil {
		return "", unable(err)
	}
	if !found {
		return "", usagef("librarian-intake: no topic prefix: --topic-prefix is not given, and main of %s has no %s", inbox.Path, ownerpkg.ConfigPath)
	}
	config, err := ownerpkg.LoadConfig(inbox)
	if err != nil {
		return "", unable(err)
	}
	if config.Repo != repository {
		return "", usagef("librarian-intake: --repository %q is not %q, the ledger repository that main:%s names", repository, config.Repo, ownerpkg.ConfigPath)
	}
	return config.TopicPrefix, nil
}

// intakeDeployment resolves, before anything is written, the host's journal of undelivered notices and the deployment's
// bare repository: with a deployment file (deploy, else ~/.config/owner/deploy.yaml), its state_dir's undelivered.jsonl
// and its bare_repo; without one, <user cache directory>/owner/undelivered/undelivered.jsonl and no bare repository. A
// named deployment file that is missing, any deployment file that does not load, and a journal the command cannot open
// for appending are exit 3.
func (s *session) intakeDeployment(deploy string) (journal, bareRepo string, err error) {
	path := deploy
	named := path != ""
	if home := strings.TrimSpace(s.getenv("HOME")); !named && home != "" {
		path = filepath.Join(home, ".config", "owner", "deploy.yaml")
	}
	found := false
	if path != "" {
		deployment, err := ownerpkg.LoadDeploy(path)
		switch {
		case err == nil && filepath.IsAbs(deployment.StateDir):
			journal, bareRepo, found = filepath.Join(deployment.StateDir, "undelivered.jsonl"), deployment.BareRepo, true
		case err == nil:
			return "", "", unable(fmt.Errorf("%s: state_dir %q is not an absolute path", path, deployment.StateDir))
		case named || !errors.Is(err, os.ErrNotExist):
			return "", "", unable(fmt.Errorf("the deployment file: %w", err))
		}
	}
	if !found {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", "", unable(fmt.Errorf("no deployment file and no user cache directory for the notice journal: %w", err))
		}
		journal = filepath.Join(cache, "owner", "undelivered", "undelivered.jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		return "", "", unable(fmt.Errorf("the notice journal: %w", err))
	}
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", unable(fmt.Errorf("the notice journal: %w", err))
	}
	return journal, bareRepo, file.Close()
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

func samePath(left, right string) bool {
	l, errL := filepath.EvalSymlinks(left)
	r, errR := filepath.EvalSymlinks(right)
	return errL == nil && errR == nil && filepath.Clean(l) == filepath.Clean(r)
}
