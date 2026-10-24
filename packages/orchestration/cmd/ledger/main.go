package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const usage = "usage: ledger [--dsn-file PATH] [--json] <command> ..."

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

type checkResultError struct {
	result ledger.CheckResult
	exit   int
}

func (e *checkResultError) Error() string { return "candidate check did not pass" }

type readyList []ledger.ReadyRecord

func (records readyList) String() string {
	var out strings.Builder
	for i, record := range records {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "%s owner=%s generation=%d head=%s ready_event=%d ready_at=%s",
			record.ID, record.Owner, record.Generation, record.Head, record.ReadyEvent, record.ReadyAt.Format(time.RFC3339Nano))
	}
	return out.String()
}

// lease is a granted or renewed lease. It prints as ledger.Lease in JSON, and as one line without --json: a supervised
// lease says so, because it has no end to print.
type lease ledger.Lease

func (l lease) String() string {
	end := "supervised"
	if l.ExpiresAt != nil {
		end = "expires_at=" + l.ExpiresAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%s holder=%s generation=%d %s", l.ID, l.Holder, l.Generation, end)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	jsonOutput := false
	var dsnFile string
	for len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			fmt.Fprintln(stdout, usage)
			return 0
		case "--json":
			jsonOutput = true
			args = args[1:]
		case "--dsn-file":
			if len(args) < 2 {
				return writeError(stderr, jsonOutput, &usageError{"--dsn-file needs a path"})
			}
			dsnFile, args = args[1], args[2:]
		default:
			goto parsed
		}
	}
parsed:
	if len(args) == 0 {
		return writeError(stderr, jsonOutput, &usageError{"missing command"})
	}
	dsn := getenv("LEDGER_DSN")
	if dsnFile != "" {
		contents, err := os.ReadFile(dsnFile)
		if err != nil {
			return writeError(stderr, jsonOutput, fmt.Errorf("read DSN file: %w", err))
		}
		dsn = strings.TrimSpace(string(contents))
	}
	if dsn == "" {
		return writeError(stderr, jsonOutput, &usageError{"LEDGER_DSN or --dsn-file is required"})
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return writeError(stderr, jsonOutput, err)
	}
	defer store.Close()

	value, read, err := dispatch(ctx, store, args)
	if err != nil {
		var check *checkResultError
		if errors.As(err, &check) {
			if encodeErr := writeResult(stdout, check.result, jsonOutput || read); encodeErr != nil {
				fmt.Fprintln(stderr, encodeErr)
				return 4
			}
			return check.exit
		}
		var refusal *ledger.Refusal
		if jsonOutput && errors.As(err, &refusal) {
			if encodeErr := json.NewEncoder(stdout).Encode(refusal); encodeErr != nil {
				fmt.Fprintln(stderr, encodeErr)
				return 4
			}
			return 1
		}
		return writeError(stderr, jsonOutput, err)
	}
	if value != nil {
		if err := writeResult(stdout, value, jsonOutput || read); err != nil {
			fmt.Fprintln(stderr, err)
			return 4
		}
	}
	return 0
}

// writeResult prints a command's result. With --json, and for a read command in either mode, it prints one line of
// JSON. Otherwise it prints the result's text form when it has one (a lease line, the ready list), and else the same
// JSON indented, so text output is never Go syntax.
func writeResult(stdout io.Writer, value any, asJSON bool) error {
	encoder := json.NewEncoder(stdout)
	if !asJSON {
		if text, ok := value.(fmt.Stringer); ok {
			_, err := fmt.Fprintln(stdout, text)
			return err
		}
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func dispatch(ctx context.Context, s *ledger.Store, args []string) (any, bool, error) {
	command := args[0]
	args = args[1:]
	switch command {
	case "repo":
		if len(args) < 3 || args[0] != "ensure" || (len(args) != 3 && (len(args) != 5 || args[3] != "--upstream-owner")) {
			return nil, false, usagef("repo ensure <repo> <max-rounds> [--upstream-owner <name>]")
		}
		limit, err := roundLimit(args[2])
		if err != nil {
			return nil, false, err
		}
		options := []ledger.EnsureRepoOption{}
		if len(args) == 5 {
			options = append(options, ledger.WithUpstreamOwner(args[4]))
		}
		return mutation(s.EnsureRepo(ctx, args[1], limit, options...))
	case "enqueue":
		if len(args) != 3 || args[0] == "" || args[1] == "" || args[2] == "" {
			return nil, false, usagef("enqueue <repo> <item> <owner>")
		}
		return nil, false, s.Enqueue(ctx, args[0], args[1], args[2])
	case "owners":
		if len(args) != 1 {
			return nil, true, usagef("owners <repo>")
		}
		value, err := openOwners(ctx, s, args[0])
		return value, true, err
	case "refuse-owner":
		if len(args) != 4 {
			return nil, false, usagef("refuse-owner <repo> <owner> <revision> <reason>")
		}
		revision, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return nil, false, usagef("invalid revision %q: want an integer", args[2])
		}
		return mutation(s.RecordOwnerRefused(ctx, args[0], args[1], revision, args[3]))
	case "claim":
		if len(args) != 4 {
			return nil, false, usagef("claim <repo> <item> <holder> <ttl|0>")
		}
		ttl, err := leaseLength(args[3])
		if err != nil {
			return nil, false, err
		}
		value, err := s.Claim(ctx, args[0], args[1], args[2], ttl)
		return lease(value), false, err
	case "release-holder":
		if len(args) != 3 {
			return nil, false, usagef("release-holder <repo> <holder> <reason>")
		}
		return mutation(s.ReleaseHolder(ctx, args[0], args[1], args[2]))
	case "renew":
		if len(args) != 5 {
			return nil, false, usagef("renew <repo> <item> <holder> <generation> <ttl>")
		}
		generation, err := integer(args[3])
		if err != nil {
			return nil, false, err
		}
		ttl, err := duration(args[4])
		if err != nil {
			return nil, false, err
		}
		value, err := s.Renew(ctx, args[0], args[1], args[2], generation, ttl)
		return lease(value), false, err
	case "release":
		if len(args) != 4 {
			return nil, false, usagef("release <repo> <item> <holder> <generation>")
		}
		generation, err := integer(args[3])
		if err != nil {
			return nil, false, err
		}
		return nil, false, s.Release(ctx, args[0], args[1], args[2], generation)
	case "complete":
		if len(args) != 5 {
			return nil, false, usagef("complete <repo> <item> <holder> <generation> <result>")
		}
		generation, err := integer(args[3])
		if err != nil {
			return nil, false, err
		}
		return nil, false, s.Complete(ctx, args[0], args[1], args[2], generation, args[4])
	case "expire":
		if len(args) != 1 {
			return nil, false, usagef("expire <repo>")
		}
		return mutation(s.Expire(ctx, args[0]))
	case "item":
		if len(args) != 2 {
			return nil, true, usagef("item <repo> <item>")
		}
		value, err := s.Item(ctx, args[0], args[1])
		return value, true, err
	case "items":
		if len(args) != 3 || args[1] != "--owner" {
			return nil, true, usagef("items <repo> --owner <key>")
		}
		value, err := s.OpenItems(ctx, args[0], args[2])
		return value, true, err
	case "open":
		if request, jsonInput, err := requestFile[ledger.OpenRequest](args); jsonInput {
			if err != nil {
				return nil, false, err
			}
			return mutation(s.OpenMigration(ctx, request))
		}
		if len(args) < 5 {
			return nil, false, usagef("open <repo> <id> <owner> <base> <branch> --write <path> [--write <path>] [--read <name=hash>]")
		}
		writes, reads, err := parseWritesAndReads(args[5:])
		if err != nil {
			return nil, false, err
		}
		if len(writes) == 0 {
			return nil, false, usagef("open needs at least one --write")
		}
		return mutation(s.OpenMigration(ctx, ledger.OpenRequest{
			Repo: args[0], ID: args[1], Owner: args[2], BaseCommit: args[3], Branch: args[4], Writes: writes, Reads: reads,
		}))
	case "extend":
		if request, jsonInput, err := requestFile[extendRequest](args); jsonInput {
			if err != nil {
				return nil, false, err
			}
			return mutation(s.ExtendMigration(ctx, request.Repo, request.ID, request.Generation, request.Writes, request.Reads))
		}
		if len(args) < 3 {
			return nil, false, usagef("extend <repo> <id> <generation> [--write <path>] [--read <name=hash>]")
		}
		generation, err := integer(args[2])
		if err != nil {
			return nil, false, err
		}
		writes, reads, err := parseWritesAndReads(args[3:])
		if err != nil {
			return nil, false, err
		}
		if len(writes) == 0 && len(reads) == 0 {
			return nil, false, usagef("extend needs --write or --read")
		}
		return mutation(s.ExtendMigration(ctx, args[0], args[1], generation, writes, reads))
	case "takeover":
		if len(args) != 4 {
			return nil, false, usagef("takeover <repo> <id> <owner> <generation>")
		}
		generation, err := integer(args[3])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.TakeOver(ctx, args[0], args[1], args[2], generation))
	case "ready":
		var request ledger.ReadyRequest
		if decoded, jsonInput, err := requestFile[ledger.ReadyRequest](args); jsonInput {
			if err != nil {
				return nil, false, err
			}
			request = decoded
		} else {
			if len(args) < 3 {
				return nil, false, usagef("ready <repo> <id> <generation> --head <sha> [--changed-contract <name=hash>] " +
					"[--compatibility-proof <name>] [--reviewed-head <sha>] [--review <contract=reviewer@head>] " +
					"[--approved-pinned <name>] [--upstream <commit>]")
			}
			generation, err := integer(args[2])
			if err != nil {
				return nil, false, err
			}
			request, err = parseReadyFlags(args[3:])
			if err != nil {
				return nil, false, err
			}
			request.Repo, request.ID, request.Generation = args[0], args[1], generation
		}
		return mutation(s.Ready(ctx, request))
	case "ready-list":
		if len(args) != 1 {
			return nil, false, usagef("ready-list <repo>")
		}
		value, err := s.ReadyMigrations(ctx, args[0])
		return readyList(value), false, err
	case "check":
		var request ledger.CheckRequest
		if decoded, jsonInput, err := requestFile[ledger.CheckRequest](args); jsonInput {
			if err != nil {
				return nil, true, err
			}
			request = decoded
		} else {
			if len(args) < 3 {
				return nil, true, usagef("check <repo> <id> <generation> [--path <path>|--no-paths] [--current <name=hash>] " +
					"[--commit <sha>] [--approved-pinned <name>] [--defer-reads] [--changed-contract <name=hash>] " +
					"[--head <commit>] [--compatibility-proof <name>] [--reviewed-head <commit>] " +
					"[--review <contract=reviewer@head>] [--upstream <commit>]")
			}
			generation, err := integer(args[2])
			if err != nil {
				return nil, true, err
			}
			request, err = parseCheckFlags(args[3:])
			if err != nil {
				return nil, true, err
			}
			request.Repo, request.ID, request.Generation = args[0], args[1], generation
		}
		value, err := s.Check(ctx, request)
		if err != nil {
			return value, true, err
		}
		switch value.Outcome {
		case ledger.CheckOutcomeRefused:
			return value, true, &checkResultError{result: value, exit: 1}
		case ledger.CheckOutcomeIncomplete:
			return value, true, &checkResultError{result: value, exit: 5}
		default:
			return value, true, nil
		}
	case "land":
		request, err := parseLandRequest(args)
		if err != nil {
			return nil, false, err
		}
		return mutation(s.Land(ctx, request))
	case "abandon":
		if len(args) != 4 {
			return nil, false, usagef("abandon <repo> <id> <generation> <reason>")
		}
		generation, err := integer(args[2])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.Abandon(ctx, args[0], args[1], generation, args[3]))
	case "round":
		var request ledger.RoundRequest
		if decoded, jsonInput, err := requestFile[ledger.RoundRequest](args); jsonInput {
			if err != nil {
				return nil, false, err
			}
			request = decoded
		} else {
			if len(args) < 4 {
				return nil, false, usagef("round <repo> <id> <generation> <reason> --key <key> [--batch <batch>] [--detail <detail>] [--log <log>]")
			}
			generation, err := integer(args[2])
			if err != nil {
				return nil, false, err
			}
			request, err = parseRoundFlags(args[4:])
			if err != nil {
				return nil, false, err
			}
			request.Repo, request.ID, request.Generation, request.Reason = args[0], args[1], generation, args[3]
		}
		return mutation(s.RecordRound(ctx, request))
	case "contract":
		return contractCommand(ctx, s, args)
	case "agree":
		if request, jsonInput, err := requestFile[agreementRequest](args); jsonInput {
			if err != nil {
				return nil, false, err
			}
			return mutation(s.RecordAgreement(ctx, request.Repo, request.ID, request.Parties, request.Reads))
		}
		if len(args) < 2 {
			return nil, false, usagef("agree <repo> <id> --party <party> [--party <party>] --read <contract=version>")
		}
		parties, reads, err := parseAgreement(args[2:])
		if err != nil {
			return nil, false, err
		}
		if len(parties) == 0 {
			return nil, false, usagef("agree needs at least one --party")
		}
		return mutation(s.RecordAgreement(ctx, args[0], args[1], parties, reads))
	case "stale":
		if len(args) != 1 {
			return nil, true, usagef("stale <repo>")
		}
		value, err := s.StaleAgreements(ctx, args[0])
		return value, true, err
	case "status":
		if len(args) != 1 {
			return nil, true, usagef("status <repo>")
		}
		value, err := s.Status(ctx, args[0])
		return value, true, err
	case "events":
		if len(args) < 2 || len(args) > 3 {
			return nil, true, usagef("events <repo> <after> [subject]")
		}
		after, err := integer(args[1])
		if err != nil {
			return nil, true, err
		}
		subject := ""
		if len(args) == 3 {
			subject = args[2]
		}
		value, err := s.Events(ctx, args[0], after, subject)
		return value, true, err
	default:
		return nil, false, usagef("unknown command %q", command)
	}
}

func contractCommand(ctx context.Context, s *ledger.Store, args []string) (any, bool, error) {
	if len(args) == 0 {
		return nil, false, usagef("contract register|transfer|show|list ...")
	}
	switch args[0] {
	case "register":
		if len(args) != 6 {
			return nil, false, usagef("contract register <repo> <name> <owner> <hash> <pinned>")
		}
		pinned, err := strconv.ParseBool(args[5])
		if err != nil {
			return nil, false, usagef("pinned must be true or false")
		}
		return mutation(s.RegisterContract(ctx, args[1], args[2], args[3], args[4], pinned))
	case "transfer":
		if len(args) != 5 {
			return nil, false, usagef("contract transfer <repo> <name> <from-owner> <to-owner>")
		}
		return mutation(s.TransferContract(ctx, args[1], args[2], args[3], args[4]))
	case "show":
		if len(args) != 3 {
			return nil, true, usagef("contract show <repo> <name>")
		}
		value, err := s.Contract(ctx, args[1], args[2])
		return value, true, err
	case "list":
		if len(args) != 2 {
			return nil, true, usagef("contract list <repo>")
		}
		value, err := s.Contracts(ctx, args[1])
		return value, true, err
	default:
		return nil, false, usagef("unknown contract command %q", args[0])
	}
}

func writeError(stderr io.Writer, jsonOutput bool, err error) int {
	var refusal *ledger.Refusal
	if errors.As(err, &refusal) {
		if jsonOutput {
			_ = json.NewEncoder(stderr).Encode(refusal)
		} else {
			fmt.Fprintln(stderr, refusal)
		}
		return 1
	}
	var notFound *ledger.NotFoundError
	if errors.As(err, &notFound) {
		fmt.Fprintln(stderr, notFound)
		return 3
	}
	var invalid *ledger.InvalidError
	var usage *usageError
	if errors.As(err, &invalid) || errors.As(err, &usage) {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintln(stderr, err)
	return 4
}

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

func integer(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, usagef("invalid integer %q", value)
	}
	return n, nil
}
func duration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, usagef("invalid duration %q", value)
	}
	return d, nil
}

// leaseLength is a claim's lease length: the literal 0 is a supervised lease with no end, anything else a positive
// duration. A duration that is zero, such as 0s, is refused rather than read as supervised, so a computed length that
// comes out zero is an error and not a lease that only a supervisor can free.
func leaseLength(value string) (time.Duration, error) {
	if value == "0" {
		return 0, nil
	}
	d, err := duration(value)
	if err == nil && d == 0 {
		return 0, usagef("lease length %q is zero; give 0 for a supervised lease, or a positive duration", value)
	}
	return d, err
}

// roundLimit parses a repository's landing-round limit into the range of the schema's integer column.
func roundLimit(value string) (int, error) {
	limit, err := strconv.ParseInt(value, 10, 32)
	if err != nil || limit < 0 {
		return 0, usagef("max rounds %q must be in 0..2147483647", value)
	}
	return int(limit), nil
}

// ownerLine is one owner in `ledger owners`: an owner with open work, and its refusal at its current work revision,
// when it has one.
type ownerLine struct {
	Owner   string               `json:"owner"`
	Refusal *ledger.OwnerRefusal `json:"refusal,omitempty"`
}

func openOwners(ctx context.Context, s *ledger.Store, repo string) ([]ownerLine, error) {
	owners, err := s.OpenOwners(ctx, repo)
	if err != nil {
		return nil, err
	}
	refusals, err := s.OwnerRefusals(ctx, repo)
	if err != nil {
		return nil, err
	}
	byOwner := make(map[string]*ledger.OwnerRefusal, len(refusals))
	for i := range refusals {
		byOwner[refusals[i].Owner] = &refusals[i]
	}
	lines := make([]ownerLine, len(owners))
	for i, owner := range owners {
		lines[i] = ownerLine{Owner: owner, Refusal: byOwner[owner]}
	}
	return lines, nil
}

type extendRequest struct {
	Repo       string            `json:"repo"`
	ID         string            `json:"id"`
	Generation int64             `json:"generation"`
	Writes     []string          `json:"writes"`
	Reads      map[string]string `json:"reads"`
}

type agreementRequest struct {
	Repo    string           `json:"repo"`
	ID      string           `json:"id"`
	Parties []string         `json:"parties"`
	Reads   map[string]int64 `json:"reads"`
}

func requestFile[T any](args []string) (T, bool, error) {
	var request T
	if len(args) == 0 || args[0] != "--request-file" {
		return request, false, nil
	}
	if len(args) != 2 {
		return request, true, usagef("--request-file needs one path")
	}
	contents, err := os.ReadFile(args[1])
	if err != nil {
		return request, true, fmt.Errorf("read request file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, true, usagef("invalid request JSON: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return request, true, usagef("invalid request JSON: request file must contain one JSON value")
	}
	return request, true, nil
}
func parsePair(value string, allowEmpty bool) (string, string, error) {
	name, content, ok := strings.Cut(value, "=")
	if !ok || name == "" || (!allowEmpty && content == "") {
		return "", "", usagef("invalid name=value %q", value)
	}
	if strings.Contains(content, "=") {
		return "", "", usagef("ambiguous name=value %q; use --request-file", value)
	}
	return name, content, nil
}

func parseWritesAndReads(args []string) ([]string, map[string]string, error) {
	writes := []string{}
	reads := map[string]string{}
	for len(args) > 0 {
		if len(args) < 2 {
			return nil, nil, usagef("list flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--write":
			writes = append(writes, value)
		case "--read":
			name, hash, err := parsePair(value, false)
			if err != nil {
				return nil, nil, err
			}
			reads[name] = hash
		default:
			return nil, nil, usagef("unknown list flag %q", flag)
		}
	}
	return writes, reads, nil
}

// parseCheckFlags reads a check's candidate: its changed paths, current read hashes, changed contracts and statement
// records. The records are single values, kept verbatim as --path keeps its value.
func parseCheckFlags(args []string) (ledger.CheckRequest, error) {
	request := ledger.CheckRequest{Current: map[string]string{}, ChangedContracts: map[string]string{}}
	sawPath, noPaths := false, false
	for len(args) > 0 {
		switch args[0] {
		case "--no-paths":
			noPaths = true
			args = args[1:]
			continue
		case "--defer-reads":
			request.DeferReads = true
			args = args[1:]
			continue
		}
		if len(args) < 2 {
			return ledger.CheckRequest{}, usagef("check flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--path":
			sawPath = true
			request.Changed = append(request.Changed, value)
		case "--current":
			name, hash, err := parsePair(value, true)
			if err != nil {
				return ledger.CheckRequest{}, err
			}
			request.Current[name] = hash
		case "--commit":
			request.Commit = value
		case "--approved-pinned":
			request.ApprovedPinned = append(request.ApprovedPinned, value)
		case "--changed-contract":
			name, hash, err := parsePair(value, false)
			if err != nil {
				return ledger.CheckRequest{}, err
			}
			request.ChangedContracts[name] = hash
		case "--head":
			request.Head = value
		case "--compatibility-proof":
			request.CompatibilityProof = value
		case "--reviewed-head":
			request.ReviewedHead = value
		case "--review":
			review, err := parseReview(value)
			if err != nil {
				return ledger.CheckRequest{}, err
			}
			request.Reviews = append(request.Reviews, review)
		case "--upstream":
			request.Upstream = value
		default:
			return ledger.CheckRequest{}, usagef("unknown check flag %q", flag)
		}
	}
	if sawPath && noPaths {
		return ledger.CheckRequest{}, usagef("check takes --path or --no-paths, not both")
	}
	if noPaths {
		request.Changed = []string{}
	}
	return request, nil
}

func parseRoundFlags(args []string) (ledger.RoundRequest, error) {
	var request ledger.RoundRequest
	for len(args) > 0 {
		if len(args) < 2 {
			return ledger.RoundRequest{}, usagef("round flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--key":
			request.Key = value
		case "--batch":
			request.Batch = value
		case "--detail":
			request.Detail = value
		case "--log":
			request.Log = value
		default:
			return ledger.RoundRequest{}, usagef("unknown round flag %q", flag)
		}
	}
	if request.Key == "" {
		return ledger.RoundRequest{}, usagef("round needs --key")
	}
	return request, nil
}

func parseReadyFlags(args []string) (ledger.ReadyRequest, error) {
	request := ledger.ReadyRequest{ChangedContracts: map[string]string{}}
	for len(args) > 0 {
		if len(args) < 2 {
			return ledger.ReadyRequest{}, usagef("ready flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--head":
			request.Head = value
		case "--changed-contract":
			name, hash, err := parsePair(value, false)
			if err != nil {
				return ledger.ReadyRequest{}, err
			}
			if strings.ContainsAny(name, "=@") {
				return ledger.ReadyRequest{}, usagef("ambiguous name=value %q; use --request-file", value)
			}
			request.ChangedContracts[name] = hash
		case "--compatibility-proof":
			request.CompatibilityProof = value
		case "--reviewed-head":
			request.ReviewedHead = value
		case "--review":
			review, err := parseReview(value)
			if err != nil {
				return ledger.ReadyRequest{}, err
			}
			request.Reviews = append(request.Reviews, review)
		case "--approved-pinned":
			if value == "" || strings.ContainsAny(value, "=@") {
				return ledger.ReadyRequest{}, usagef("ambiguous name %q; use --request-file", value)
			}
			request.ApprovedPinned = append(request.ApprovedPinned, value)
		case "--upstream":
			request.Upstream = value
		default:
			return ledger.ReadyRequest{}, usagef("unknown ready flag %q", flag)
		}
	}
	if request.Head == "" {
		return ledger.ReadyRequest{}, usagef("ready needs --head")
	}
	return request, nil
}

func parseReview(value string) (ledger.Review, error) {
	contract, reviewerHead, contractOK := strings.Cut(value, "=")
	reviewer, head, headOK := strings.Cut(reviewerHead, "@")
	if !contractOK || !headOK || contract == "" || reviewer == "" || head == "" ||
		strings.ContainsAny(contract, "=@") || strings.ContainsAny(reviewer, "=@") || strings.ContainsAny(head, "=@") {
		return ledger.Review{}, usagef("ambiguous review %q; use --request-file", value)
	}
	return ledger.Review{Contract: contract, Reviewer: reviewer, Head: head}, nil
}

func parseAgreement(args []string) ([]string, map[string]int64, error) {
	parties := []string{}
	reads := map[string]int64{}
	for len(args) > 0 {
		if len(args) < 2 {
			return nil, nil, usagef("list flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--party":
			parties = append(parties, value)
		case "--read":
			name, versionText, err := parsePair(value, false)
			if err != nil {
				return nil, nil, err
			}
			version, err := integer(versionText)
			if err != nil {
				return nil, nil, err
			}
			reads[name] = version
		default:
			return nil, nil, usagef("unknown list flag %q", flag)
		}
	}
	return parties, reads, nil
}

func mutation[T any](value T, err error) (any, bool, error) { return value, false, err }

func parseLandRequest(args []string) (ledger.LandRequest, error) {
	if request, jsonInput, err := requestFile[ledger.LandRequest](args); jsonInput {
		return request, err
	}
	request := ledger.LandRequest{
		Current:          map[string]string{},
		ChangedContracts: map[string]string{},
	}
	// --no-paths distinguishes an empty candidate from an omitted changed-path list.
	sawPath, noPaths := false, false
	for len(args) > 0 {
		if args[0] == "--no-paths" {
			noPaths = true
			args = args[1:]
			continue
		}
		if len(args) < 2 {
			return ledger.LandRequest{}, usagef("land flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--repo":
			request.Repo = value
		case "--migration":
			request.Migration = value
		case "--generation":
			generation, err := integer(value)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.Generation = generation
		case "--commit":
			request.Commit = value
		case "--path":
			sawPath = true
			request.ChangedPaths = append(request.ChangedPaths, value)
		case "--current":
			name, hash, err := parsePair(value, true)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.Current[name] = hash
		case "--changed-contract":
			name, hash, err := parsePair(value, false)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.ChangedContracts[name] = hash
		case "--approved-pinned":
			request.ApprovedPinned = append(request.ApprovedPinned, value)
		case "--head":
			request.Head = value
		case "--compatibility-proof":
			request.CompatibilityProof = value
		case "--reviewed-head":
			request.ReviewedHead = value
		case "--review":
			review, err := parseReview(value)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.Reviews = append(request.Reviews, review)
		case "--upstream":
			request.Upstream = value
		default:
			return ledger.LandRequest{}, usagef("unknown land flag %q", flag)
		}
	}
	if request.Repo == "" || request.Migration == "" {
		return ledger.LandRequest{}, usagef("land needs --repo and --migration")
	}
	if sawPath && noPaths {
		return ledger.LandRequest{}, usagef("land takes --path or --no-paths, not both")
	}
	if noPaths {
		request.ChangedPaths = []string{}
	}
	return request, nil
}
