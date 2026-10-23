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
		limit, err := integer(args[2])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.EnsureRepo(ctx, args[1], int(limit)))
	case "claim":
		if len(args) != 4 {
			return nil, false, usagef("claim <repo> <item> <holder> <ttl>")
		}
		ttl, err := duration(args[3])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.Claim(ctx, args[0], args[1], args[2], ttl))
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
		if len(args) < 6 || len(args) > 7 {
			return nil, false, usagef("open <repo> <id> <owner> <base> <branch> <writes> [reads]")
		}
		writes := list(args[5])
		reads := map[string]string{}
		if len(args) == 7 {
			var err error
			reads, err = stringMap(args[6], false)
			if err != nil {
				return nil, false, err
			}
		}
		return mutation(s.OpenMigration(ctx, ledger.OpenRequest{Repo: args[0], ID: args[1], Owner: args[2], BaseCommit: args[3], Branch: args[4], Writes: writes, Reads: reads}))
	case "extend":
		if len(args) < 4 || len(args) > 5 {
			return nil, false, usagef("extend <repo> <id> <generation> <writes> [reads]")
		}
		generation, err := integer(args[2])
		if err != nil {
			return nil, false, err
		}
		reads := map[string]string{}
		if len(args) == 5 {
			reads, err = stringMap(args[4], false)
			if err != nil {
				return nil, false, err
			}
		}
		return mutation(s.ExtendMigration(ctx, args[0], args[1], generation, list(args[3]), reads))
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
		if len(args) < 4 || len(args) > 5 {
			return nil, true, usagef("check <repo> <id> <generation> <changed> [current]")
		}
		generation, err := integer(args[2])
		if err != nil {
			return nil, true, err
		}
		current := map[string]string{}
		if len(args) == 5 {
			current, err = stringMap(args[4], true)
			if err != nil {
				return nil, true, err
			}
		}
		value, err := s.Check(ctx, ledger.CheckRequest{Repo: args[0], ID: args[1], Generation: generation, Changed: list(args[3]), Current: current})
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
		if len(args) != 4 {
			return nil, false, usagef("agree <repo> <id> <parties> <reads>")
		}
		reads, err := versionMap(args[3])
		if err != nil {
			return nil, false, err
		}
		return mutation(s.RecordAgreement(ctx, args[0], args[1], list(args[2]), reads))
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
func list(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}
func stringMap(value string, allowEmpty bool) (map[string]string, error) {
	out := map[string]string{}
	if value == "" {
		return out, nil
	}
	for _, pair := range strings.Split(value, ",") {
		name, content, ok := strings.Cut(pair, "=")
		if !ok || name == "" || (!allowEmpty && content == "") {
			return nil, usagef("invalid name=value list %q", value)
		}
		out[name] = content
	}
	return out, nil
}
func versionMap(value string) (map[string]int64, error) {
	values, err := stringMap(value, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(values))
	for name, text := range values {
		version, err := integer(text)
		if err != nil {
			return nil, err
		}
		out[name] = version
	}
	return out, nil
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
		Current:          map[string]string{},
		ChangedContracts: map[string]string{},
	}
	var hasPaths, hasCurrent bool
	for len(args) > 0 {
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
		case "--changed-paths":
			request.ChangedPaths = list(value)
			hasPaths = true
		case "--current":
			current, err := stringMap(value, true)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.Current = current
			hasCurrent = true
		case "--changed-contracts":
			contracts, err := stringMap(value, false)
			if err != nil {
				return ledger.LandRequest{}, err
			}
			request.ChangedContracts = contracts
		case "--approved-pinned":
			request.ApprovedPinned = list(value)
		default:
			return ledger.LandRequest{}, usagef("unknown land flag %q", flag)
		}
	}
	if request.Repo == "" || request.Migration == "" || request.Commit == "" || !hasPaths || !hasCurrent {
		return ledger.LandRequest{}, usagef("land needs --repo, --migration, --commit, --changed-paths and --current")
	}
	return request, nil
}
