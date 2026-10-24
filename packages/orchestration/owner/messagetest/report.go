package messagetest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The owner's progress at which a kill can come: before it claims the item, after it claims it, or after it answers.
const (
	MomentBeforeClaim = "before-claim"
	MomentAfterClaim  = "after-claim"
	MomentAfterAnswer = "after-answer"
)

// Report is the message test's result: DIR/report.json, and DIR/report.md for a person.
type Report struct {
	Run        string        `json:"run"`
	Prefix     string        `json:"prefix"`
	Nodes      []string      `json:"nodes"`
	Edge       string        `json:"edge"`
	Seed       int64         `json:"seed"`
	Out        string        `json:"out"`
	Daemon     Daemon        `json:"daemon"`
	Revisions  Revisions     `json:"revisions"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Seconds    float64       `json:"seconds"`
	Steps      []StepTiming  `json:"steps"`
	Panes      []PaneCheck   `json:"panes"`
	Published  []Publication `json:"published"`
	Deliveries []Latency     `json:"deliveries"`
	KillLog    []Kill        `json:"kills"`
	Restart    *Restart      `json:"restart,omitempty"`
	Expected   Expected      `json:"expected"`
	Failures   []Failure     `json:"failures"`
	Passed     bool          `json:"passed"`
}

// Daemon names the supervisor the run started, where its configuration, socket and log are, and the omp its panes
// run, which the driver gives it as LEGION_OMP_PATH.
type Daemon struct {
	Executable string `json:"executable"`
	Config     string `json:"config"`
	Socket     string `json:"tmux_socket"`
	Log        string `json:"log"`
	Port       int    `json:"port"`
	Omp        string `json:"omp"`
}

// StepTiming is one step of the run and its duration. Error is why it did not finish.
type StepTiming struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	Seconds   float64   `json:"seconds"`
	Error     string    `json:"error,omitempty"`
}

// PaneCheck is an owner pane the supervisor started: how long it took after the start item, the launch variables of
// its process, the directory it works in and the topics it recorded.
type PaneCheck struct {
	Owner        string            `json:"owner"`
	Pane         string            `json:"pane"`
	PID          int               `json:"pid"`
	StartSeconds float64           `json:"start_seconds"`
	Env          map[string]string `json:"env"`
	Dir          string            `json:"dir"`
	Topics       []string          `json:"topics"`
}

// Publication is one notice the driver published and the owners that subscribe to its topic.
type Publication struct {
	Step      string    `json:"step"`
	Kind      string    `json:"kind"`
	Topic     string    `json:"topic"`
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	At        time.Time `json:"at"`
	Receivers []string  `json:"receivers"`
}

// Latency is the time from a notice's publication to the owner's record of it.
type Latency struct {
	Owner   string  `json:"owner"`
	ID      string  `json:"id"`
	Topic   string  `json:"topic"`
	Seconds float64 `json:"seconds"`
}

// Kill is one owner pane the driver killed, at the drawn moment of the owner's progress and the one it observed.
// RelaunchSeconds is how long the supervisor took to start the owner again, or 0 when the item was already done.
type Kill struct {
	Item            string    `json:"item"`
	Drawn           string    `json:"drawn"`
	Observed        string    `json:"observed"`
	Pane            string    `json:"pane"`
	At              time.Time `json:"at"`
	RelaunchSeconds float64   `json:"relaunch_seconds"`
}

// Restart is the daemon restart while an owner ran: how long the stop and the start took and when its item finished.
type Restart struct {
	Item         string   `json:"item"`
	StopSeconds  float64  `json:"stop_seconds"`
	StartSeconds float64  `json:"start_seconds"`
	ItemSeconds  float64  `json:"item_seconds"`
	PanesBefore  []string `json:"panes_before"`
	PanesAfter   []string `json:"panes_after"`
}

// Finish records the failures and the verdict: the run passes only when no assertion failed.
func (r *Report) Finish(failures []Failure) {
	r.Failures = append([]Failure{}, failures...)
	r.Passed = len(failures) == 0
	if !r.FinishedAt.IsZero() && !r.StartedAt.IsZero() {
		r.Seconds = r.FinishedAt.Sub(r.StartedAt).Seconds()
	}
}

// Write writes report.json and report.md in dir.
func (r Report) Write(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.markdown()), 0o644)
}

func (r Report) markdown() string {
	var b strings.Builder
	verdict := "PASSED"
	if !r.Passed {
		verdict = fmt.Sprintf("FAILED (%d failures)", len(r.Failures))
	}
	fmt.Fprintf(&b, "# Owner message test %s\n\n", r.Run)
	fmt.Fprintf(&b, "- Result: **%s**\n- Duration: %.1f s (%s to %s)\n- Prefix: `%s`\n- Nodes: %s\n- Edge: `%s`\n- Kill seed: %d\n",
		verdict, r.Seconds, r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339), r.Prefix, code(r.Nodes), r.Edge, r.Seed)
	fmt.Fprintf(&b, "- Daemon: `%s start --config %s` (port %d, tmux socket `%s`, log `%s`), panes running `%s`\n\n", r.Daemon.Executable, r.Daemon.Config, r.Daemon.Port, r.Daemon.Socket, r.Daemon.Log, r.Daemon.Omp)
	r.Revisions.markdown(&b)
	b.WriteString("## Steps\n\n| Step | Seconds | Error |\n| --- | --- | --- |\n")
	for _, step := range r.Steps {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", step.Name, seconds(step.Seconds), cell(step.Error))
	}
	b.WriteString("\n## Owner panes\n\n| Owner | Pane | PID | Start (s) | Launch variables | Directory |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, pane := range r.Panes {
		names := make([]string, 0, len(pane.Env))
		for name := range pane.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		vars := make([]string, 0, len(names))
		for _, name := range names {
			vars = append(vars, fmt.Sprintf("`%s=%s`", name, pane.Env[name]))
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s | `%s` |\n", pane.Owner, pane.Pane, pane.PID, seconds(pane.StartSeconds), strings.Join(vars, "<br>"), pane.Dir)
	}
	b.WriteString("\n## Notices\n\n| Step | Kind | Topic | Event | Receivers |\n| --- | --- | --- | --- | --- |\n")
	for _, p := range r.Published {
		fmt.Fprintf(&b, "| %s | %s | `%s` | `%s` | %s |\n", p.Step, p.Kind, p.Topic, p.ID, code(p.Receivers))
	}
	b.WriteString("\n## Deliveries\n\n| Owner | Event | Topic | Latency (s) |\n| --- | --- | --- | --- |\n")
	for _, d := range r.Deliveries {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", d.Owner, d.ID, d.Topic, seconds(d.Seconds))
	}
	b.WriteString("\n## Kills\n\n| Item | Drawn | Observed | Pane | Relaunch (s) |\n| --- | --- | --- | --- | --- |\n")
	for _, k := range r.KillLog {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", k.Item, k.Drawn, k.Observed, k.Pane, seconds(k.RelaunchSeconds))
	}
	if r.Restart != nil {
		fmt.Fprintf(&b, "\n## Daemon restart\n\nItem `%s`: stop %s s, start %s s, item done %s s after the restart. Panes before %s, after %s.\n",
			r.Restart.Item, seconds(r.Restart.StopSeconds), seconds(r.Restart.StartSeconds), seconds(r.Restart.ItemSeconds),
			code(r.Restart.PanesBefore), code(r.Restart.PanesAfter))
	}
	b.WriteString("\n## Failures\n\n")
	if len(r.Failures) == 0 {
		b.WriteString("None.\n")
		return b.String()
	}
	b.WriteString("| Kind | Subject | Detail |\n| --- | --- | --- |\n")
	for _, f := range r.Failures {
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", f.Kind, f.Subject, cell(f.Detail))
	}
	return b.String()
}

func seconds(value float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", value), "0"), ".")
}

func code(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "`"+value+"`")
	}
	return strings.Join(quoted, ", ")
}

func cell(text string) string {
	return strings.NewReplacer("|", `\|`, "\n", "<br>").Replace(text)
}

func (r Revisions) markdown(b *strings.Builder) {
	owner := r.Owner
	revision := "revision unknown (built without VCS stamping)"
	if owner.Revision != "" {
		revision = fmt.Sprintf("revision `%s` (commit time %s, modified %s)", owner.Revision, owner.Time, owner.Modified)
	}
	fmt.Fprintf(b, "## Revisions\n\n- Owner: `%s` %s, %s, built with %s\n", owner.Path, owner.Version, revision, owner.GoVersion)
	modules := make([]string, 0, len(r.Dependencies))
	for _, module := range r.Dependencies {
		modules = append(modules, "`"+module.Path+" "+module.Version+"`")
	}
	fmt.Fprintf(b, "- Go modules: %s\n", strings.Join(modules, ", "))
	for _, tool := range []struct{ name, output string }{
		{"legion version", r.Legion}, {"omp --version", r.Omp}, {"tmux -V", r.Tmux}, {"git --version", r.Git},
	} {
		fmt.Fprintf(b, "- `%s`: %s\n", tool.name, cell(tool.output))
	}
	names := make([]string, 0, len(r.Graph))
	for name := range r.Graph {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(b, "- Graph input `%s`: `%s`\n", name, r.Graph[name])
	}
	b.WriteString("\n")
}
