package graphcheck

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

// compileFindings runs C1 (README.md, "The rules"): a changed module that the build builds and Lean refuses is a
// certain build failure, so each declaration Lean's errors name is one refusal, at the error's position, with the
// kind the landing program's build blame would give it. A module whose errors name nothing leaves the check
// unable: the batch gets Lean's reason, never a silent pass.
//
// Only the modules the build builds are compiled, so only they are refused here: the build never reads a changed
// module that no build target imports, and a scratch compile of one could refuse a change the build accepts.
func compileFindings(c *Candidate, cfg Config) ([]landing.Finding, error) {
	var out []landing.Finding
	for _, ch := range c.Changes {
		m := c.Module(ch.Module)
		if m == nil || m.State != Failed || !c.Built(ch.Module) {
			continue
		}
		findings, err := refusals(m, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
	}
	return out, nil
}

// refusals turns one module's compile output into one refusal per declaration that Lean's errors name. A weak name
// (the namespace of a dotted name, a type that a mismatch mentions) is left out, as the build's blame leaves it out
// when a primary name is there: a graph finding is never weak, and a module whose errors give only weak names is
// unable.
func refusals(m *Module, cfg Config) ([]landing.Finding, error) {
	reason := "C1: " + firstLine(leanReason(m))
	var out []landing.Finding
	named := map[string]bool{}
	for _, e := range m.Errors {
		for _, block := range blame.Parse(blameLog(m.Module, e), cfg.Roots).Blocks {
			for _, name := range blame.Names(block.Message) {
				if name.Kind == blame.Weak || named[name.Value] {
					continue
				}
				named[name.Value] = true
				out = append(out, landing.Finding{Path: errorPath(m, e), Line: max(e.Line, 1), Col: max(e.Col, 1),
					Verdict: landing.VerdictRefuse, Kind: name.Kind, Declaration: name.Value, Reason: reason})
			}
		}
	}
	if len(out) == 0 {
		return nil, unable("%s does not compile and Lean's output names no declaration: %s", m.Path, leanOutput(m))
	}
	return out, nil
}

// leanOutput is what Lean said, on one line, for a reason the landing program records: the command it ran is the
// first line and says nothing, so the end of the output is kept, where a diagnostic is.
func leanOutput(m *Module) string {
	text := strings.TrimSpace(m.Output)
	if text == "" {
		return "Lean wrote nothing"
	}
	const keep = 2000
	if len(text) > keep {
		text = "…" + text[len(text)-keep:]
	}
	return strings.Join(strings.Fields(text), " ")
}

// blameLog renders one of Lean's errors as a build log carries it, which is what blame.Parse reads: the module
// being built, then the error with its file and position. The names and kinds then come from the landing
// program's own reader, so a graph refusal and a build refusal name a declaration the same way.
func blameLog(module string, e LeanError) string {
	return fmt.Sprintf("✖ [1/1] Building %s\nerror: %s:%d:%d: %s\n", module, e.File, e.Line, e.Col, e.Message)
}

// leanReason is what the module's compile said: its first error, else its whole output.
func leanReason(m *Module) string {
	if len(m.Errors) != 0 {
		return m.Errors[0].Message
	}
	if out := strings.TrimSpace(m.Output); out != "" {
		return out
	}
	return "Lean failed and said nothing"
}

// errorPath is the repository-relative file of an error: the one Lean named below the scratch directory, else
// the module's own file.
func errorPath(m *Module, e LeanError) string {
	if e.File == "" || strings.HasPrefix(e.File, "/") {
		return m.Path
	}
	return e.File
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
