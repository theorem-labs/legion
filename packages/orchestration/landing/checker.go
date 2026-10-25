package landing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// The verdicts of a graph finding.
const (
	VerdictRefuse = "refuse"
	VerdictFlag   = "flag"
)

// The output formats of a check command.
const (
	FormatFindings = "findings" // path<TAB>reason lines, the default
	FormatGraph    = "graph"    // graph finding lines (FormatGraphFinding)
)

// CommandChecker runs one configured policy command. The command receives the
// candidate tree in LAND_TREE and the batch's base commit in LAND_BASE, and
// writes its findings to stdout in Format.
type CommandChecker struct {
	CheckName string
	Command   []string
	Directory string
	Format    string // FormatFindings (the default when empty) or FormatGraph
}

// Name returns the configured policy name.
func (c CommandChecker) Name() string {
	return c.CheckName
}

// Check runs the command and parses its stdout. For the graph format, exit 3
// returns a *CheckUnableError whose reason is stderr; any other failure returns
// an error that holds stderr. Exit 0 with anything on stderr is an error too,
// for both formats, so a finding written to stderr is never dropped in silence.
func (c CommandChecker) Check(ctx context.Context, in CheckInput) ([]Finding, error) {
	if len(c.Command) == 0 {
		return nil, fmt.Errorf("check %q has no command", c.CheckName)
	}
	graph := c.Format == FormatGraph
	command := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	command.Dir = c.Directory
	command.Env = append(os.Environ(), "LAND_TREE="+string(in.Tree), "LAND_BASE="+in.Base)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	message := strings.TrimSpace(stderr.String())
	if err != nil {
		var exit *exec.ExitError
		if graph && errors.As(err, &exit) && exit.ExitCode() == 3 {
			return nil, &CheckUnableError{Check: c.CheckName, Reason: message}
		}
		return nil, fmt.Errorf("run check %q: %w: %s", c.CheckName, err, message)
	}
	if message != "" {
		return nil, fmt.Errorf("check %q exited 0 with stderr: %s", c.CheckName, message)
	}
	if graph {
		return parseGraphFindings(stdout.String())
	}
	return parseFindings(stdout.String())
}

func parseFindings(output string) ([]Finding, error) {
	var findings []Finding
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		path, reason, found := strings.Cut(line, "\t")
		if !found || path == "" || reason == "" {
			return nil, fmt.Errorf("check output line %d is not path and reason", lineNumber+1)
		}
		findings = append(findings, Finding{Path: path, Reason: reason, Verdict: VerdictRefuse})
	}
	return findings, nil
}

func parseGraphFindings(output string) ([]Finding, error) {
	var findings []Finding
	for lineNumber, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		finding, err := ParseGraphFinding(line)
		if err != nil {
			return nil, fmt.Errorf("check output line %d: %w", lineNumber+1, err)
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

// FormatGraphFinding writes one graph check output line:
//
//	<verdict>\t<kind>\t<declaration>\t<path>:<line>:<col>\t<rule>: <message>
//
// It refuses an unknown verdict or kind, an empty declaration, path or reason,
// a position below 1, and a field holding a tab or a newline.
func FormatGraphFinding(f Finding) (string, error) {
	if err := validGraphFinding(f); err != nil {
		return "", err
	}
	return strings.Join([]string{f.Verdict, string(f.Kind), f.Declaration, f.Path + ":" + strconv.Itoa(f.Line) + ":" + strconv.Itoa(f.Col), f.Reason}, "\t"), nil
}

// ParseGraphFinding reads one graph check output line, with the same refusals
// as FormatGraphFinding.
func ParseGraphFinding(line string) (Finding, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		return Finding{}, fmt.Errorf("graph finding %q has %d tab-separated fields, want 5", line, len(fields))
	}
	position := fields[3]
	colAt := strings.LastIndexByte(position, ':')
	if colAt < 0 {
		return Finding{}, fmt.Errorf("graph finding position %q is not path:line:col", position)
	}
	lineAt := strings.LastIndexByte(position[:colAt], ':')
	if lineAt < 0 {
		return Finding{}, fmt.Errorf("graph finding position %q is not path:line:col", position)
	}
	lineNumber, lineErr := strconv.Atoi(position[lineAt+1 : colAt])
	col, colErr := strconv.Atoi(position[colAt+1:])
	if lineErr != nil || colErr != nil {
		return Finding{}, fmt.Errorf("graph finding position %q is not path:line:col", position)
	}
	f := Finding{Verdict: fields[0], Kind: blame.Kind(fields[1]), Declaration: fields[2], Path: position[:lineAt], Line: lineNumber, Col: col, Reason: fields[4]}
	if err := validGraphFinding(f); err != nil {
		return Finding{}, err
	}
	return f, nil
}

func validGraphFinding(f Finding) error {
	switch {
	case f.Verdict != VerdictRefuse && f.Verdict != VerdictFlag:
		return fmt.Errorf("graph finding verdict %q is not %s or %s", f.Verdict, VerdictRefuse, VerdictFlag)
	case f.Kind != blame.Unknown && f.Kind != blame.Added && f.Kind != blame.Changed && f.Kind != blame.Field:
		return fmt.Errorf("graph finding kind %q is not unknown, added, changed or field", f.Kind)
	case f.Declaration == "" || f.Path == "" || f.Reason == "":
		return fmt.Errorf("graph finding has an empty declaration, path or reason")
	case f.Line < 1 || f.Col < 1:
		return fmt.Errorf("graph finding position %d:%d is below 1", f.Line, f.Col)
	case strings.ContainsAny(f.Declaration+f.Path+f.Reason, "\t\n"):
		return fmt.Errorf("graph finding field holds a tab or a newline")
	}
	return nil
}
