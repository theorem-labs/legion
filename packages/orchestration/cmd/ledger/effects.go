package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/orchestration/ledger"
)

const (
	effectsUsage        = "effects <repo> <batch>"
	beginWithdrawUsage  = "begin-withdraw <repo> <batch>"
	effectAttemptUsage  = "effect-attempt <repo> <batch> <key> --result-file <json> --escalate-to <owner> --escalate-after <n>"
	effectDoneUsage     = "effect-done <repo> <batch> <key> --result-file <json> [--then-file <json>] [--output <name>=<file>]... [--escalate-to <owner>]"
	effectOutputsUsage  = "effect-outputs <repo> <batch> <key> --dir <dir>"
	effectApplyUsage    = "effect-apply-contracts <repo> <batch> <key>"
	effectSkipUsage     = "effect-skip <repo> <batch> <key> --reason <text>"
	effectReleaseUsage  = "effect-release <repo> <batch> <key> --reason <text>"
	outputFlagUsage     = "--output <name>=<file>"
	repeatableOutputArg = "--output"
)

// safeOutputName is the rule every stored output name must meet before effect-outputs writes any file: one path
// element that starts with a letter or a digit, so it has no separator, is not . or .., and is not hidden. The rule
// protects the directory the command writes to; the ledger stores whatever name a done sends.
var safeOutputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// usageOf is a usage error that names a command's form.
func usageOf(form string) error { return &usageError{form} }

// effectCommand runs one effect command. Every effect command prints JSON.
func effectCommand(ctx context.Context, s *ledger.Store, command string, args []string) (any, error) {
	switch command {
	case "effects":
		if len(args) != 2 {
			return nil, usageOf(effectsUsage)
		}
		return s.BatchEffects(ctx, args[0], args[1])
	case "begin-withdraw":
		if len(args) != 2 {
			return nil, usageOf(beginWithdrawUsage)
		}
		return s.BeginWithdraw(ctx, args[0], args[1])
	case "effect-attempt":
		flags, err := effectFlags(args, effectAttemptUsage, "--result-file", "--escalate-to", "--escalate-after")
		if err != nil {
			return nil, err
		}
		if len(flags) != 3 {
			return nil, usageOf(effectAttemptUsage)
		}
		threshold, err := canonicalInteger("--escalate-after", flags["--escalate-after"][0])
		if err != nil {
			return nil, err
		}
		if threshold < 0 {
			return nil, usagef("--escalate-after %d must not be negative", threshold)
		}
		result, err := readJSONFile[json.RawMessage](flags["--result-file"][0])
		if err != nil {
			return nil, err
		}
		return s.BatchEffectAttempt(ctx, args[0], args[1], args[2], ledger.AttemptRequest{
			Result: result, EscalateTo: flags["--escalate-to"][0], EscalateAfter: threshold,
		})
	case "effect-done":
		flags, err := effectFlags(args, effectDoneUsage, "--result-file", "--then-file", repeatableOutputArg, "--escalate-to")
		if err != nil {
			return nil, err
		}
		if flags["--result-file"] == nil {
			return nil, usageOf(effectDoneUsage)
		}
		request, err := doneRequest(flags)
		if err != nil {
			return nil, err
		}
		return s.BatchEffectDone(ctx, args[0], args[1], args[2], request)
	case "effect-outputs":
		if len(args) != 5 || args[3] != "--dir" {
			return nil, usageOf(effectOutputsUsage)
		}
		outputs, err := s.BatchEffectOutputs(ctx, args[0], args[1], args[2])
		if err != nil {
			return nil, err
		}
		return writeOutputs(args[4], outputs)
	case "effect-apply-contracts":
		if len(args) != 3 {
			return nil, usageOf(effectApplyUsage)
		}
		return s.BatchEffectApplyContracts(ctx, args[0], args[1], args[2])
	case "effect-skip":
		if len(args) != 5 || args[3] != "--reason" {
			return nil, usageOf(effectSkipUsage)
		}
		return s.BatchEffectSkip(ctx, args[0], args[1], args[2], args[4])
	case "effect-release":
		if len(args) != 5 || args[3] != "--reason" {
			return nil, usageOf(effectReleaseUsage)
		}
		return s.BatchEffectRelease(ctx, args[0], args[1], args[2], args[4])
	}
	return nil, usagef("unknown command %q", command)
}

// effectFlags reads `<repo> <batch> <key>` and then flag-value pairs, each named in allowed. Only --output may repeat.
func effectFlags(args []string, form string, allowed ...string) (map[string][]string, error) {
	if len(args) < 3 || (len(args)-3)%2 != 0 {
		return nil, usageOf(form)
	}
	flags := map[string][]string{}
	for i := 3; i < len(args); i += 2 {
		flag, value := args[i], args[i+1]
		if !slices.Contains(allowed, flag) {
			return nil, usagef("unknown flag %q: %s", flag, form)
		}
		if flags[flag] != nil && flag != repeatableOutputArg {
			return nil, usagef("%s given twice: %s", flag, form)
		}
		flags[flag] = append(flags[flag], value)
	}
	return flags, nil
}

// doneRequest builds effect-done's request from its flags: the result, the follow-ons, each output file's bytes and the
// escalation owner.
func doneRequest(flags map[string][]string) (ledger.DoneRequest, error) {
	result, err := readJSONFile[json.RawMessage](flags["--result-file"][0])
	if err != nil {
		return ledger.DoneRequest{}, err
	}
	request := ledger.DoneRequest{Result: result}
	if then := flags["--then-file"]; then != nil {
		if request.Then, err = readJSONFile[[]ledger.FollowOn](then[0]); err != nil {
			return ledger.DoneRequest{}, err
		}
	}
	for _, output := range flags[repeatableOutputArg] {
		// An output's name never holds "=" under effect-outputs' rule, so the first "=" ends it and the file's path
		// may hold more.
		name, file, ok := strings.Cut(output, "=")
		if !ok || name == "" || file == "" {
			return ledger.DoneRequest{}, usagef("invalid %s %q", outputFlagUsage, output)
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return ledger.DoneRequest{}, usagef("read output %s: %v", name, err)
		}
		request.Outputs = append(request.Outputs, ledger.OutputFile{Name: name, Bytes: contents})
	}
	if escalateTo := flags["--escalate-to"]; escalateTo != nil {
		request.EscalateTo = escalateTo[0]
	}
	return request, nil
}

// readJSONFile decodes the one JSON value that the file at path holds into a T, refusing fields that T does not have.
func readJSONFile[T any](path string) (T, error) {
	var value T
	contents, err := os.ReadFile(path)
	if err != nil {
		return value, usagef("read %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, usagef("invalid JSON in %s: %v", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, usagef("invalid JSON in %s: the file must contain one JSON value", path)
	}
	return value, nil
}

// writeOutputs writes each output as dir/<name> with O_CREAT|O_EXCL|O_NOFOLLOW and mode 0600, and returns what it
// wrote. Before any write it requires every name to be a safe output name and dir to be an existing empty directory,
// so it never writes outside dir or over a file or a symlink that is there.
func writeOutputs(dir string, outputs []ledger.EffectOutput) ([]ledger.OutputDigest, error) {
	for _, output := range outputs {
		if !safeOutputName.MatchString(output.Name) {
			return nil, usagef("stored output %q is not a safe output name (%s); nothing was written", output.Name, safeOutputName)
		}
	}
	entries, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return nil, usagef("--dir %s is not empty; nothing was written", dir)
	}
	written := make([]ledger.OutputDigest, 0, len(outputs))
	for _, output := range outputs {
		file, err := os.OpenFile(filepath.Join(dir, output.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return written, err
		}
		_, err = file.Write(output.Bytes)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return written, fmt.Errorf("write output %s: %w", output.Name, err)
		}
		written = append(written, ledger.OutputDigest{Name: output.Name, SHA256: output.SHA256})
	}
	return written, nil
}

// readDir lists dir, which must be an existing directory: anything else is a usage error.
func readDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return nil, usagef("--dir %s: %v; nothing was written", dir, pathError.Err)
	}
	return entries, err
}
