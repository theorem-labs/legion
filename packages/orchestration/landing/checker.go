package landing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CommandChecker runs one configured policy command. The command receives the
// candidate tree in LAND_TREE and writes path<TAB>reason findings to stdout.
type CommandChecker struct {
	CheckName string
	Command   []string
	Directory string
}

// Name returns the configured policy name.
func (c CommandChecker) Name() string {
	return c.CheckName
}

// Check runs the command and parses its complete findings.
func (c CommandChecker) Check(ctx context.Context, tree Tree) ([]Finding, error) {
	if len(c.Command) == 0 {
		return nil, fmt.Errorf("check %q has no command", c.CheckName)
	}
	command := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	command.Dir = c.Directory
	command.Env = append(os.Environ(), "LAND_TREE="+string(tree))
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run check %q: %w: %s", c.CheckName, err, strings.TrimSpace(string(output)))
	}
	return parseFindings(string(output))
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
		findings = append(findings, Finding{Path: path, Reason: reason})
	}
	return findings, nil
}
