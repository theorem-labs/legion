package main

import (
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

type checkRefusal struct{ result ledger.CheckResult }

func (e *checkRefusal) Error() string { return "candidate check refused" }

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
		var check *checkRefusal
		if errors.As(err, &check) {
			if jsonOutput || read {
				if encodeErr := json.NewEncoder(stdout).Encode(check.result); encodeErr != nil {
					fmt.Fprintln(stderr, encodeErr)
					return 4
				}
			} else {
				fmt.Fprintln(stdout, check.result)
			}
			return 1
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
		if jsonOutput || read {
			if err := json.NewEncoder(stdout).Encode(value); err != nil {
				fmt.Fprintln(stderr, err)
				return 4
			}
		} else {
			fmt.Fprintln(stdout, value)
		}
	}
	return 0
}

func dispatch(ctx context.Context, s *ledger.Store, args []string) (any, bool, error) {
	command := args[0]
	args = args[1:]
	switch command {
	case "repo":
		if len(args) != 3 || args[0] != "ensure" {
			return nil, false, usagef("repo ensure <repo> <max-rounds>")
		}
		limit, err := roundLimit(args[2])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.EnsureRepo(ctx, args[1], limit))
	case "enqueue":
		if len(args) != 3 || args[0] == "" || args[1] == "" || args[2] == "" {
			return nil, false, usagef("enqueue <repo> <item> <owner>")
		}
		return nil, false, s.Enqueue(ctx, args[0], args[1], args[2])
	case "owners":
		if len(args) != 1 {
			return nil, true, usagef("owners <repo>")
		}
		value, err := s.OpenOwners(ctx, args[0])
		return value, true, err
	case "claim":
		if len(args) != 4 {
			return nil, false, usagef("claim <repo> <item> <holder> <ttl|0>")
		}
		ttl, err := leaseLength(args[3])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.Claim(ctx, args[0], args[1], args[2], ttl))
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
		return mutation(s.Renew(ctx, args[0], args[1], args[2], generation, ttl))
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
	case "check":
		var request ledger.CheckRequest
		if decoded, jsonInput, err := requestFile[ledger.CheckRequest](args); jsonInput {
			if err != nil {
				return nil, true, err
			}
			request = decoded
		} else {
			if len(args) < 3 {
				return nil, true, usagef("check <repo> <id> <generation> [--path <path>] [--current <name=hash>]")
			}
			generation, err := integer(args[2])
			if err != nil {
				return nil, true, err
			}
			paths, current, err := parsePathsAndCurrent(args[3:])
			if err != nil {
				return nil, true, err
			}
			request = ledger.CheckRequest{Repo: args[0], ID: args[1], Generation: generation, Changed: paths, Current: current}
		}
		value, err := s.Check(ctx, request)
		if err != nil {
			return value, true, err
		}
		if !value.Pass {
			return value, true, &checkRefusal{result: value}
		}
		return value, true, nil
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
		if len(args) != 4 {
			return nil, false, usagef("round <repo> <id> <generation> <reason>")
		}
		generation, err := integer(args[2])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.RecordRound(ctx, args[0], args[1], generation, args[3]))
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
		return nil, false, usagef("contract register|transfer|show ...")
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

// leaseLength is a claim's lease length: 0 is a supervised lease with no end, anything else a
// duration.
func leaseLength(value string) (time.Duration, error) {
	if value == "0" {
		return 0, nil
	}
	return duration(value)
}

// roundLimit parses a repository's landing-round limit into the range of the schema's integer column.
func roundLimit(value string) (int, error) {
	limit, err := strconv.ParseInt(value, 10, 32)
	if err != nil || limit < 0 {
		return 0, usagef("max rounds %q must be in 0..2147483647", value)
	}
	return int(limit), nil
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
	if err := json.Unmarshal(contents, &request); err != nil {
		return request, true, usagef("invalid request JSON: %v", err)
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

func parsePathsAndCurrent(args []string) ([]string, map[string]string, error) {
	paths := []string{}
	current := map[string]string{}
	for len(args) > 0 {
		if len(args) < 2 {
			return nil, nil, usagef("list flags need values")
		}
		flag, value := args[0], args[1]
		args = args[2:]
		switch flag {
		case "--path":
			paths = append(paths, value)
		case "--current":
			name, hash, err := parsePair(value, true)
			if err != nil {
				return nil, nil, err
			}
			current[name] = hash
		default:
			return nil, nil, usagef("unknown list flag %q", flag)
		}
	}
	return paths, current, nil
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
	if len(args) == 2 && args[0] == "--request-file" {
		contents, err := os.ReadFile(args[1])
		if err != nil {
			return ledger.LandRequest{}, fmt.Errorf("read land request: %w", err)
		}
		var request ledger.LandRequest
		if err := json.Unmarshal(contents, &request); err != nil {
			return ledger.LandRequest{}, usagef("invalid land request JSON: %v", err)
		}
		return request, nil
	}
	request := ledger.LandRequest{
		ChangedPaths:     []string{},
		Current:          map[string]string{},
		ChangedContracts: map[string]string{},
	}
	// A candidate that changes nothing must say so: an omitted path list is never read as an empty one.
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
		default:
			return ledger.LandRequest{}, usagef("unknown land flag %q", flag)
		}
	}
	if request.Repo == "" || request.Migration == "" || request.Commit == "" {
		return ledger.LandRequest{}, usagef("land needs --repo, --migration and --commit")
	}
	switch {
	case sawPath && noPaths:
		return ledger.LandRequest{}, usagef("land takes --path or --no-paths, not both")
	case !sawPath && !noPaths:
		return ledger.LandRequest{}, usagef("land needs the candidate's changed files: one --path for each, or --no-paths when it changes none")
	}
	return request, nil
}
