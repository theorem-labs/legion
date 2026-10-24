package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"github.com/sjawhar/legion/orchestration/owner/messagetest"
	"gopkg.in/yaml.v3"
)

const messageTestUsage = "message-test [--prefix P] [--nodes A,B] [--kills N] --out DIR | message-test topics | " +
	"message-test subscribed | message-test saw <delivery id> <topic> | message-test answer <item>"

// parseMessageTest reads `owner message-test`: a test owner's command, or the driver's options.
func parseMessageTest(args []string) (func(*session) (any, error), error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		flags, err := parseDriverFlags(args)
		if err != nil {
			return nil, err
		}
		return func(s *session) (any, error) { return s.messageTestRun(flags) }, nil
	}
	name, rest := args[0], args[1:]
	switch name {
	case "topics", "subscribed":
		if len(rest) != 0 {
			return nil, usagef("message-test %s takes no arguments", name)
		}
		if name == "topics" {
			return (*session).messageTestTopics, nil
		}
		return (*session).messageTestSubscribed, nil
	case "saw":
		if len(rest) != 2 || strings.TrimSpace(rest[0]) == "" || strings.TrimSpace(rest[1]) == "" {
			return nil, usagef("message-test saw <delivery id> <topic>")
		}
		return func(s *session) (any, error) { return s.messageTestSaw(rest[0], rest[1]) }, nil
	case "answer":
		if len(rest) != 1 {
			return nil, usagef("message-test answer <item>")
		}
		ref, err := ownerpkg.ParseItemID(rest[0])
		if err != nil || (ref.Type != "start" && ref.Type != "need" && ref.Type != "offer") {
			return nil, usagef("message-test answer takes a start:, need: or offer: item, not %q", rest[0])
		}
		return func(s *session) (any, error) { return s.messageTestAnswer(rest[0], ref) }, nil
	}
	return nil, usagef("unknown message-test command %q; %s", name, messageTestUsage)
}

type driverFlags struct {
	prefix string
	nodes  []string
	kills  int
	out    string
}

func parseDriverFlags(args []string) (driverFlags, error) {
	flags := driverFlags{kills: 5}
	for len(args) > 0 {
		if len(args) < 2 {
			return driverFlags{}, usagef("%s needs a value; %s", args[0], messageTestUsage)
		}
		name, value := args[0], args[1]
		args = args[2:]
		switch name {
		case "--prefix":
			if !ownerpkg.ValidTopicPrefix(value) {
				return driverFlags{}, usagef("--prefix %q is not a prefix under notifications. that ends in a dot", value)
			}
			flags.prefix = value
		case "--nodes":
			nodes := strings.Split(value, ",")
			if len(nodes) != 2 || strings.TrimSpace(nodes[0]) == "" || strings.TrimSpace(nodes[1]) == "" || nodes[0] == nodes[1] {
				return driverFlags{}, usagef("--nodes takes two different node ids, A,B")
			}
			flags.nodes = nodes
		case "--kills":
			kills, err := strconv.Atoi(value)
			if err != nil || kills < 0 {
				return driverFlags{}, usagef("--kills takes a count, not %q", value)
			}
			flags.kills = kills
		case "--out":
			flags.out = value
		default:
			return driverFlags{}, usagef("unknown message-test option %s; %s", name, messageTestUsage)
		}
	}
	if strings.TrimSpace(flags.out) == "" {
		return driverFlags{}, usagef("message-test needs --out DIR; %s", messageTestUsage)
	}
	return flags, nil
}

// messageTestTopicsOf is `owner topics` plus the topics the driver gave this owner to subscribe to as well.
func (s *session) messageTestTopicsOf() ([]string, error) {
	value, err := s.topics()
	if err != nil {
		return nil, err
	}
	topics := value.(topicsResult).Topics
	extra, err := messagetest.LoadExtraTopics(s.deploy.StateDir)
	if err != nil {
		return nil, unable(err)
	}
	for _, topic := range extra[s.owner] {
		if !containsString(topics, topic) {
			topics = append(topics, topic)
		}
	}
	return topics, nil
}

func (s *session) messageTestTopics() (any, error) {
	topics, err := s.messageTestTopicsOf()
	if err != nil {
		return nil, err
	}
	return topicsResult{Owner: s.owner, Topics: topics}, nil
}

// messageTestSubscribed records the topics the owner subscribed to. The report shows it beside the listener's record of
// the session's subscriptions, which is what the driver waits for before it publishes.
func (s *session) messageTestSubscribed() (any, error) {
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	topics, err := s.messageTestTopicsOf()
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(messagetest.Subscribed{Owner: p.ID, Topics: topics})
	if err != nil {
		return nil, err
	}
	path := messagetest.SubscribedRecord(s.config.RecordsRoot, p.ID)
	previous, found, err := p.tree.BlobAtHead(path)
	if err != nil {
		return nil, err
	}
	outcome, err := s.runStep(p, ownerpkg.Step{Path: path, Data: data, Subject: "message-test: subscribed"})
	if err != nil {
		return nil, err
	}
	return stepResult{AlreadyDone: found && bytes.Equal(previous, data), Record: path, Commit: outcome.Commit}, nil
}

// messageTestSaw records one delivery, once: a second run for the same delivery id records nothing.
func (s *session) messageTestSaw(delivery, topic string) (any, error) {
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	path := messagetest.SawRecord(s.config.RecordsRoot, p.ID, delivery)
	if _, found, err := p.tree.BlobAtHead(path); err != nil {
		return nil, err
	} else if found {
		commit, err := p.tree.LastCommitTouching(path)
		return stepResult{AlreadyDone: true, Record: path, Commit: commit}, err
	}
	data, err := yaml.Marshal(messagetest.Saw{Owner: p.ID, Delivery: delivery, Topic: topic, ReceivedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	trailers := map[string][]string{messagetest.DeliveryTrailer: {delivery}, messagetest.TopicTrailer: {topic}}
	if _, err := p.tree.CommitFile(s.ctx, path, data, "message-test: saw "+delivery, trailers); err != nil {
		return nil, err
	}
	commit, err := p.tree.LastCommitTouching(path)
	return stepResult{Record: path, Commit: commit}, err
}

// messageTestAnswer answers a start, need or offer item with an offer record of fixed text, as one step: the record
// with the item's Owner-Item trailer, the follow-up offer of a need, and the completion. The record has a revision, as
// a spec does: 1, and one more each time a newer version rewrites it. A need's follow-up is numbered as production's
// offer numbers it, offer:<edge>@r<n> with n the revision of the record that answers the need. An item older than the
// version the owner already answered, which the answer record holds, completes as superseded:<newer version> with no
// record.
func (s *session) messageTestAnswer(item string, ref ownerpkg.ItemRef) (any, error) {
	p, err := s.party()
	if err != nil {
		return nil, err
	}
	store, err := s.ledger()
	if err != nil {
		return nil, err
	}
	current, err := store.Item(s.ctx, s.repo, item)
	if err != nil {
		return nil, err
	}
	if current.Owner != p.ID {
		return nil, refusef("%s is in the queue of %s, not of %s", item, current.Owner, p.ID)
	}
	if current.State == "done" {
		return current, nil
	}
	path := messagetest.AnswerRecord(s.config.RecordsRoot, p.ID, ref.Type, ref.Subject)
	previous, found, err := p.tree.BlobAtHead(path)
	if err != nil {
		return nil, err
	}
	answer := messagetest.Answer{Item: item, Type: ref.Type, Subject: ref.Subject, Version: ref.Version, Revision: 1, Text: messagetest.AnswerText}
	if found {
		var answered messagetest.Answer
		if err := yaml.Unmarshal(previous, &answered); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if versionAfter(answered.Version, ref.Version) {
			return s.completeSuperseded(item, answered.Version)
		}
		answer.Revision = answered.Revision
	}
	data, err := yaml.Marshal(answer)
	if err != nil {
		return nil, err
	}
	if found && !bytes.Equal(data, previous) {
		answer.Revision++
		if data, err = yaml.Marshal(answer); err != nil {
			return nil, err
		}
	}
	var followUps []ownerpkg.FollowUp
	if ref.Type == "need" {
		source, _, err := ownerpkg.ParseEdgeID(ref.Subject)
		if err != nil {
			return nil, err
		}
		followUps = append(followUps, ownerpkg.FollowUp{Item: fmt.Sprintf("offer:%s@r%d", ref.Subject, answer.Revision), Owner: source})
	}
	if err := s.checkNotHeldByOther(store, item); err != nil {
		return nil, err
	}
	outcome, err := s.runStep(p, ownerpkg.Step{Answers: item, Path: path, Data: data, Subject: "message-test: answer " + item, FollowUps: followUps})
	if err != nil {
		return nil, err
	}
	result := stepResult{Item: item, Answered: []string{item}, AlreadyDone: outcome.AlreadyDone, Record: path, Commit: outcome.Commit}
	for _, followUp := range followUps {
		result.FollowUps = append(result.FollowUps, followUp.Item)
	}
	return result, nil
}

// versionAfter reports whether version a is newer than b: revisions r<n> and plain counts compare as numbers.
func versionAfter(a, b string) bool {
	number := func(version string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimPrefix(version, "r"))
		return n, err == nil
	}
	x, okX := number(a)
	y, okY := number(b)
	return okX && okY && strings.HasPrefix(a, "r") == strings.HasPrefix(b, "r") && x > y
}

type messageTestResult struct {
	Report   string `json:"report"`
	Passed   bool   `json:"passed"`
	Failures int    `json:"failures"`
}

// messageTestRun drives the message test against the operator's deployment and the supervisor named legion on PATH.
// It is the operator's program, not an owner's: it reads no launch variable, and its ledger is the deployment's
// ledger_dsn_file alone, which it gives the supervisor, so the test owners get it as LEGION_LEDGER_DSN_FILE.
func (s *session) messageTestRun(flags driverFlags) (any, error) {
	deployPath := s.deployPath
	if deployPath == "" {
		home := strings.TrimSpace(s.getenv("HOME"))
		if home == "" {
			return nil, usagef("--deploy is not given and HOME is unset")
		}
		deployPath = filepath.Join(home, ".config", "owner", "deploy.yaml")
	}
	deploy, err := ownerpkg.LoadDeploy(deployPath)
	if err != nil {
		return nil, err
	}
	out := flags.out
	if !filepath.IsAbs(out) {
		cwd, err := filepath.Abs(s.cwd)
		if err != nil {
			return nil, err
		}
		out = filepath.Join(cwd, out)
	}
	owner, err := executablePath(os.Args[0], s.getenv("PATH"))
	if err != nil {
		return nil, unable(fmt.Errorf("find the owner executable that the owners run: %w", err))
	}
	legion, err := messagetest.LookPath("legion", s.getenv("PATH"))
	if err != nil {
		return nil, unable(fmt.Errorf("find the supervisor: %w", err))
	}
	// SIGINT or SIGTERM ends the run's steps: the driver then stops what it started and writes a partial report.
	ctx, cancel := context.WithCancelCause(s.ctx)
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case received := <-signals:
			cancel(fmt.Errorf("interrupted by %s", received))
		case <-ctx.Done():
		}
	}()
	report, err := messagetest.Run(ctx, messagetest.Options{
		Prefix: flags.prefix, Nodes: flags.nodes, Kills: flags.kills, Out: out,
		Deploy: deploy, Owner: owner, Legion: legion,
		Environ: os.Environ(), Getenv: s.getenv, Progress: s.stderr, Timeouts: messagetest.DefaultTimeouts(),
	})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(report.Out, "report.md")
	switch {
	case report.Interrupted != "":
		return nil, refusef("the message test was %s; partial report %s", report.Interrupted, path)
	case !report.Passed:
		return nil, refusef("the message test failed with %d failures; report %s", len(report.Failures), path)
	}
	return messageTestResult{Report: path, Passed: true}, nil
}

// executablePath is this command's own path as it was run, so a link to it keeps its name: an absolute path, a
// path relative to the working directory, or a name found on PATH.
func executablePath(arg0, path string) (string, error) {
	if filepath.IsAbs(arg0) {
		return arg0, nil
	}
	if strings.Contains(arg0, string(filepath.Separator)) {
		return filepath.Abs(arg0)
	}
	return messagetest.LookPath(arg0, path)
}
