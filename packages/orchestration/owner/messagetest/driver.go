// Package messagetest is the owner message test (plan Task 1.8): before any production owner starts, it shows
// that a notice on each topic kind reaches real owner sessions that the supervisor starts from their ledger queues,
// and that duplicate notices and items, items out of order, killed owner sessions and a supervisor restart still
// leave each step done once. The driver reads every assertion from durable state: the ledger's events, the owners'
// branches, the owner sessions' own transcripts of the deliveries they took, and the listener's record of each
// session's subscriptions. No step waits on the owner model recording a delivery.
package messagetest

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
	"gopkg.in/yaml.v3"
)

// Options configure one run.
type Options struct {
	Prefix string   // under notifications. and ending in a dot, empty for the graph's own; the run's topics are under it, then the run's name
	Nodes  []string // the two owners; none means pickNodes chooses them from the graph
	Kills  int      // how many owner panes to kill, on KillSchedule; 0 is DefaultKills, and 1 or 2 is refused
	Out    string   // the run's directory, absolute and new or empty
	// AllowMissingKinds are the topic kinds (TopicKinds) the two nodes may lack: the run fails on any other kind
	// that no topic of the nodes has, and the report lists each allowed kind they lack.
	AllowMissingKinds []string

	Deploy   ownerpkg.Deploy // the operator's deployment: its graph, its ledger_dsn_file and the keys the test copies
	Owner    string          // the owner executable the test owners run
	Legion   string          // the supervisor: `legion start --config FILE`
	Environ  []string        // the supervisor's environment, before the driver puts Owner's directory on PATH
	Getenv   func(string) string
	Progress io.Writer
	Timeouts Timeouts
}

// Timeouts bound each wait of the run. Poll is the interval at which the driver reads the ledger, git and tmux.
type Timeouts struct {
	PaneStart, Subscribed, Delivery, Item, DaemonReady, DaemonStop, Poll time.Duration
}

// DefaultTimeouts are the plan's 60 s for an owner pane to start, and long waits for real model sessions.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		PaneStart: 60 * time.Second, Subscribed: 10 * time.Minute, Delivery: 5 * time.Minute, Item: 15 * time.Minute,
		DaemonReady: time.Minute, DaemonStop: 30 * time.Second, Poll: 100 * time.Millisecond,
	}
}

// Moments are the three moments of an owner's step at which the run kills its pane, in the order of the step: before
// the owner claims the item, after it claims it, and after it answers it. A run kills at least one pane at each.
var Moments = []string{MomentBeforeClaim, MomentAfterClaim, MomentAfterAnswer}

type driver struct {
	opts   Options
	report *Report

	killsStarted bool // a kill step ran, so the run's kills are checked against its schedule

	run            string // the run's name: its ledger repository and the daemon's project
	prefix         string // the run's topic prefix: the --prefix, or the graph's, then the run's name and a dot
	root           string // the copy's records root
	nodes          []string
	source, target string
	targetKind     string // the target's graph kind, by which the routing table routes its offer notices
	bare           string
	stateDir       string // the test deployment's state_dir
	deployPath     string
	daemonState    string
	daemonConfig   string
	daemonLog      string
	tmux           tmuxServer // the daemon's private tmux server, under the run's own TMUX_TMPDIR
	port           int
	tokenDir       string // the private directory of the bearer files the run wrote, removed at teardown
	omp            string // the omp the owner panes run, which the supervisor gets as LEGION_OMP_PATH

	store         *ledger.Store
	publisher     *ownerpkg.HTTPPublisher
	listener      string // the Envoy listener's URL, and its bearer
	listenerToken string
	routing       ownerpkg.Routing
	topics        ownerpkg.Topics     // the test graph's topics.json
	arrivalPicks  []arrivalTopic      // the topic of each kind the nodes hold, which the arrival step publishes on
	subscribe     map[string][]string // the topics each owner must subscribe to
	expected      Expected
	offers        map[string]string // need version → the offer its answer queues (expectAnswered)
	failures      []Failure

	// What the driver saw of the owner panes' processes: the directories each owner's session transcripts are in
	// (Oh My Pi keeps one working directory's sessions in one), the session ids of the transcripts its processes held
	// open, in the order seen, and every owner process of the run, for the teardown.
	transcriptDirs map[string][]string
	sessionIDs     map[string][]string
	ownerPIDs      map[int]string

	daemon     *exec.Cmd
	daemonDone chan error
	daemonErr  error
	exited     bool
}

// Run runs the message test and writes DIR/report.json and DIR/report.md. The error is only for options it cannot
// start with; every failure of the run is in the report, which passes only when every assertion held. When ctx ends
// before the steps do (an operator's SIGINT), the run stops its steps, collects what it can, stops everything it
// started and writes a partial report marked interrupted, with context.Cause(ctx) as the reason; collect and
// teardown run on a context of their own.
func Run(ctx context.Context, opts Options) (Report, error) {
	if err := opts.check(); err != nil {
		return Report{}, err
	}
	if err := makeOut(opts.Out); err != nil {
		return Report{}, err
	}
	now := time.Now().UTC()
	run, err := runName(now)
	if err != nil {
		return Report{}, err
	}
	if opts.Kills == 0 {
		opts.Kills = DefaultKills
	}
	d := &driver{
		opts: opts, run: run, root: "skeleton", tmux: runTmux(opts.Out, daemonProject(run)),
		report: &Report{Out: opts.Out, StartedAt: now, KillSchedule: KillSchedule(opts.Kills)},
	}
	d.report.Run = d.run
	type step struct {
		name  string
		run   func(context.Context) error
		fatal bool
	}
	steps := []step{
		{"setup", d.setup, true},
		{"start the daemon", d.startDaemon, true},
		{"wake from the ledger", d.wake, true},
		{"arrival", d.arrival, false},
		{"duplicates", d.duplicates, false},
		{"out of order", d.outOfOrder, false},
	}
	for i := range opts.Kills {
		steps = append(steps, step{fmt.Sprintf("kill %d of %d (%s)", i+1, opts.Kills, d.report.KillSchedule[i]), d.kill(i), false})
	}
	steps = append(steps, step{"daemon restart", d.restart, false})
	for _, s := range steps {
		if ctx.Err() != nil {
			break
		}
		if err := d.timed(ctx, s.name, s.run); err != nil && s.fatal {
			break
		}
	}
	if ctx.Err() != nil {
		d.report.Interrupted = context.Cause(ctx).Error()
		d.logf("interrupted (%s): collecting what the run left, then stopping what it started", d.report.Interrupted)
	}
	after, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var recorded Recorded
	_ = d.timed(after, "collect", func(ctx context.Context) error {
		var err error
		recorded, err = d.collect(ctx)
		return err
	})
	_ = d.timed(after, "teardown", d.teardown)
	failures := append(d.failures, Check(d.expected, recorded)...)
	if d.report.Interrupted != "" {
		failures = append(failures, Failure{Kind: FailInterrupted, Subject: "run", Detail: d.report.Interrupted + ": the steps after it did not run, and this report is partial"})
	} else if d.killsStarted {
		failures = append(failures, checkKills(d.report.KillSchedule, d.report.KillLog)...)
	}
	d.report.Expected = d.expected
	d.report.FinishedAt = time.Now().UTC()
	d.report.Finish(failures)
	if err := d.report.Write(opts.Out); err != nil {
		return *d.report, err
	}
	d.logf("report %s: passed %t, %d failures", filepath.Join(opts.Out, "report.md"), d.report.Passed, len(failures))
	return *d.report, nil
}

// runName is a run's name: message-test-, its UTC stamp to the second and 8 random hex digits, so that two runs
// started in the same second share no ledger repository, daemon project, tmux socket, bearer directory or topic. It
// holds only lowercase letters, digits and hyphens, which each of those names takes as it is.
func runName(at time.Time) (string, error) {
	var suffix [4]byte
	if _, err := cryptorand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("draw the run's name: %w", err)
	}
	return "message-test-" + strings.ToLower(at.UTC().Format("20060102T150405Z")) + "-" + hex.EncodeToString(suffix[:]), nil
}

// cleanupTimeout bounds the collect and teardown of a run, which run on a context of their own.
const cleanupTimeout = 2 * time.Minute

func (o Options) check() error {
	unknown := slices.DeleteFunc(slices.Clone(o.AllowMissingKinds), func(kind string) bool { return slices.Contains(TopicKinds, kind) })
	switch {
	case len(unknown) > 0:
		return fmt.Errorf("allowed missing kinds %s: the topic kinds are %s", strings.Join(unknown, ", "), strings.Join(TopicKinds, ", "))
	case o.Prefix != "" && !ownerpkg.ValidTopicPrefix(o.Prefix):
		return fmt.Errorf("prefix %q is not a prefix under notifications. that ends in a dot", o.Prefix)
	case len(o.Nodes) != 0 && (len(o.Nodes) != 2 || o.Nodes[0] == o.Nodes[1]):
		return fmt.Errorf("the message test takes two different nodes, not %v", o.Nodes)
	case o.Kills < 0 || (o.Kills > 0 && o.Kills < len(Moments)):
		return fmt.Errorf("kills %d: a run kills at least %d owner panes, one at each moment of a step (%s); 0 is the default %d", o.Kills, len(Moments), strings.Join(Moments, ", "), DefaultKills)
	case !filepath.IsAbs(o.Out) || !filepath.IsAbs(o.Owner) || !filepath.IsAbs(o.Legion) || !filepath.IsAbs(o.Deploy.LedgerDSNFile):
		return fmt.Errorf("the output directory, the owner and legion executables and the deployment's ledger_dsn_file must be absolute paths")
	case strings.ContainsAny(o.Out+o.Owner, " \t\n"):
		return fmt.Errorf("the output directory and the owner executable must not hold whitespace: the owner brief names them in one command line")
	case o.Getenv == nil || o.Progress == nil || o.Timeouts.Poll <= 0:
		return errors.New("the message test needs Getenv, Progress and Timeouts")
	}
	return nil
}

func makeOut(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("the output directory %s is not empty", dir)
	}
	return nil
}

func (d *driver) logf(format string, args ...any) {
	fmt.Fprintf(d.opts.Progress, "message-test %s %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// timed runs one step, records its duration, and records a failed step as a failure of the run.
func (d *driver) timed(ctx context.Context, name string, run func(context.Context) error) error {
	d.logf("%s", name)
	start := time.Now()
	err := run(ctx)
	timing := StepTiming{Name: name, StartedAt: start.UTC(), Seconds: time.Since(start).Seconds()}
	if err != nil {
		timing.Error = err.Error()
		d.failures = append(d.failures, Failure{Kind: FailStep, Subject: name, Detail: err.Error()})
		d.logf("%s failed: %v", name, err)
	}
	d.report.Steps = append(d.report.Steps, timing)
	return err
}

func (d *driver) fail(kind, subject, format string, args ...any) {
	d.failures = append(d.failures, Failure{Kind: kind, Subject: subject, Detail: fmt.Sprintf(format, args...)})
}

// errTimedOut is the error of a wait whose timeout passed.
var errTimedOut = errors.New("timed out")

// wait polls done until it holds, the timeout passes, or the daemon exits on its own.
func (d *driver) wait(ctx context.Context, timeout time.Duration, what string, done func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		if d.daemon != nil && !d.exited {
			select {
			case err := <-d.daemonDone:
				d.exited, d.daemonErr = true, err
				return fmt.Errorf("the daemon exited (%v) while the run waited for %s; its log %s ends:\n%s", err, what, d.daemonLog, tail(d.daemonLog))
			default:
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s waiting for %s", errTimedOut, timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.opts.Timeouts.Poll):
		}
	}
}

func (d *driver) setup(ctx context.Context) error {
	// The run's tmux socket lives under its directory: a directory too long for it stops the run before anything
	// starts.
	if err := d.tmux.fits(); err != nil {
		return err
	}
	out := d.opts.Out
	graph, err := readGraph(d.opts.Deploy.GraphDir)
	if err != nil {
		return err
	}
	d.nodes = d.opts.Nodes
	if len(d.nodes) == 0 {
		if d.nodes, err = graph.pickNodes(); err != nil {
			return err
		}
	}
	d.report.Nodes = d.nodes
	d.source, d.target = graph.edge(d.nodes)
	target, _ := graph.owners.ByID(d.target)
	d.targetKind = target.Kind
	d.report.Edge = ownerpkg.EdgeID(d.source, d.target)
	base := d.opts.Prefix
	if base == "" {
		// No prefix given: the run's topics are under the one the deployment's graph names, which its owners use.
		base = graph.topics.Prefix
	}
	// Each run's topics are under a prefix of its own, so that runs on one hub never see each other's notices.
	d.prefix = base + d.run + "."
	d.report.Prefix = d.prefix

	graphDir := filepath.Join(out, "graph")
	topics, owners, err := graph.write(graphDir, d.prefix, d.nodes, d.source)
	if err != nil {
		return err
	}
	d.topics = topics
	if err := d.pickArrivalTopics(); err != nil {
		return err
	}
	d.bare = filepath.Join(out, "repo.git")
	if err := createCopy(d.bare, filepath.Join(out, "seed"), copyConfig(d.run, d.prefix, graphDir), d.nodes); err != nil {
		return err
	}
	factOwners := ownerpkg.FactOwners{"fact-" + testFact: testFact}
	if d.routing, err = ownerpkg.NewRouting(d.prefix, factOwners); err != nil {
		return err
	}

	// The source owner also subscribes to the fact owner's and the librarian's topics, which no node owner holds.
	d.stateDir = filepath.Join(out, "state")
	extra := map[string][]string{d.source: {d.prefix + "fact." + testFact + ".needs", d.prefix + "concepts"}}
	if err := writeJSON(ExtraTopicsPath(d.stateDir), extra); err != nil {
		return err
	}
	facts := ownerpkg.Facts{testFact: {Users: []string{d.source}}}
	d.subscribe = map[string][]string{}
	for _, node := range d.nodes {
		owner, _ := owners.ByID(node)
		list, err := ownerpkg.Subscriptions(node, owner.Role, topics, facts, factOwners)
		if err != nil {
			return err
		}
		for _, topic := range extra[node] {
			if !slices.Contains(list, topic) {
				list = append(list, topic)
			}
		}
		d.subscribe[node] = list
	}

	deploy := d.opts.Deploy
	deploy.BareRepo, deploy.GraphDir, deploy.StateDir = d.bare, graphDir, d.stateDir
	deploy.SessionsDir, deploy.ScoreWork = filepath.Join(out, "sessions"), filepath.Join(out, "score")
	d.deployPath = filepath.Join(out, "deploy.yaml")
	data, err := yaml.Marshal(deploy)
	if err != nil {
		return err
	}
	if err := os.WriteFile(d.deployPath, data, 0o644); err != nil {
		return err
	}
	if _, err := ownerpkg.LoadDeploy(d.deployPath); err != nil {
		return fmt.Errorf("the test deployment: %w", err)
	}

	dsn, err := os.ReadFile(d.opts.Deploy.LedgerDSNFile)
	if err != nil {
		return fmt.Errorf("read the ledger DSN file: %w", err)
	}
	if d.store, err = ledger.Open(ctx, strings.TrimSpace(string(dsn))); err != nil {
		return fmt.Errorf("open the ledger: %w", err)
	}
	if _, err := d.store.EnsureRepo(ctx, d.run, 3); err != nil {
		return fmt.Errorf("make the ledger repository %s: %w", d.run, err)
	}

	url, token, err := ownerpkg.ResolveListener(d.opts.Getenv)
	if err != nil {
		return err
	}
	d.publisher, d.listener, d.listenerToken = ownerpkg.NewHTTPPublisher(url, token), url, token
	natsURLs, err := ownerpkg.ListenerNATSURLs(d.opts.Getenv)
	if err != nil {
		return err
	}
	daemonDir := filepath.Join(out, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		return err
	}
	// The supervisor reads its bearers from files. The ones the run writes are private, under the operator's
	// state_dir, and never in the run's directory, which the report names; the teardown removes them.
	if !filepath.IsAbs(d.opts.Deploy.StateDir) {
		return fmt.Errorf("the deployment's state_dir %q is not an absolute path", d.opts.Deploy.StateDir)
	}
	private := filepath.Join(d.opts.Deploy.StateDir, "message-test", d.run)
	if err := privateDir(private); err != nil {
		return err
	}
	d.tokenDir = private
	tokenFile, err := daemonTokenFile(d.opts.Getenv, token, private)
	if err != nil {
		return err
	}
	operatorToken, err := operatorTokenFile(private)
	if err != nil {
		return err
	}
	if d.omp, err = d.ompPath(); err != nil {
		return err
	}
	prompt := filepath.Join(daemonDir, "owner.md")
	brief, err := renderBrief(d.opts.Owner, d.deployPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(prompt, brief, 0o644); err != nil {
		return err
	}
	if d.port, err = freePort(); err != nil {
		return err
	}
	kinds := []string{}
	for _, node := range d.nodes {
		owner, _ := owners.ByID(node)
		if !slices.Contains(kinds, owner.Kind) {
			kinds = append(kinds, owner.Kind)
		}
	}
	d.daemonState = filepath.Join(daemonDir, "state")
	d.daemonConfig = filepath.Join(daemonDir, "legion.yaml")
	d.daemonLog = filepath.Join(daemonDir, "legion.log")
	// The daemon's tmux server and its socket live in the run's own tmux directory, which the teardown removes: tmux
	// uses /tmp when the directory TMUX_TMPDIR names is missing (tmux.c expand_paths), and never removes a socket file.
	if err := os.Mkdir(d.tmux.tmpDir, 0o700); err != nil {
		return err
	}
	project := daemonProject(d.run)
	supervisor := daemonConfig(project, d.daemonState, d.port, url, natsURLs, tokenFile, operatorToken, d.opts.Deploy.LedgerDSNFile, d.run, prompt, kinds)
	if data, err = yaml.Marshal(supervisor); err != nil {
		return err
	}
	if err := os.WriteFile(d.daemonConfig, data, 0o644); err != nil {
		return err
	}
	d.report.Daemon = Daemon{Executable: d.opts.Legion, Config: d.daemonConfig, Project: project, Socket: d.tmux.socket, TmuxTmpDir: d.tmux.tmpDir,
		SocketPath: d.tmux.socketPath(), Log: d.daemonLog, Port: d.port, Omp: d.omp}
	d.report.Revisions = collectRevisions(ctx, d.opts.Legion, d.omp, d.opts.Deploy.GraphDir)
	d.logf("run %s: nodes %v, edge %s, daemon config %s", d.run, d.nodes, d.report.Edge, d.daemonConfig)
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// daemonEnv is the supervisor's environment: the driver's, with daemonPath as PATH, LEGION_OMP_PATH naming the omp the
// panes run, TMUX_TMPDIR naming the run's own tmux directory (the daemon keeps that variable for every tmux command it
// runs, and names its socket legion-<project> in it), and no launch variable of the driver's own, so that a pane's
// launch variables are the supervisor's.
func (d *driver) daemonEnv() []string {
	env := slices.DeleteFunc(slices.Clone(d.opts.Environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == "PATH" || name == "LEGION_OMP_PATH" || name == "TMUX_TMPDIR" || slices.Contains(LaunchVariables, name)
	})
	return append(env, "PATH="+d.daemonPath(), "LEGION_OMP_PATH="+d.omp, "TMUX_TMPDIR="+d.tmux.tmpDir)
}

// daemonPath is the supervisor's PATH: the owner executable's directory first, then the driver's PATH.
func (d *driver) daemonPath() string {
	path := filepath.Dir(d.opts.Owner)
	for _, entry := range d.opts.Environ {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok && value != "" {
			path += string(filepath.ListSeparator) + value
		}
	}
	return path
}

// ompPath is the omp the owner panes run. The daemon never looks omp up on PATH, so the driver names it to the
// supervisor as LEGION_OMP_PATH: the operator's LEGION_OMP_PATH, else the omp on the supervisor's PATH.
func (d *driver) ompPath() (string, error) {
	named := strings.TrimSpace(d.opts.Getenv("LEGION_OMP_PATH"))
	if named == "" {
		found, err := LookPath("omp", d.daemonPath())
		if err != nil {
			return "", fmt.Errorf("find the omp the owner panes run: set LEGION_OMP_PATH, or put omp on PATH: %w", err)
		}
		return found, nil
	}
	if !filepath.IsAbs(named) {
		return "", fmt.Errorf("LEGION_OMP_PATH %q is not an absolute path", named)
	}
	if info, err := os.Stat(named); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("LEGION_OMP_PATH %q is not an executable file (%v)", named, err)
	}
	return named, nil
}

// startDaemon runs `legion start --config FILE` with its output in daemon/legion.log, and waits until it listens on
// its port.
func (d *driver) startDaemon(ctx context.Context) error {
	log, err := os.OpenFile(d.daemonLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	fmt.Fprintf(log, "--- message-test %s: %s start --config %s\n", time.Now().UTC().Format(time.RFC3339Nano), d.opts.Legion, d.daemonConfig)
	command := exec.Command(d.opts.Legion, "start", "--config", d.daemonConfig)
	command.Env = d.daemonEnv()
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", d.opts.Legion, err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	d.daemon, d.daemonDone, d.exited, d.daemonErr = command, done, false, nil
	d.report.Teardown.DaemonPIDs = append(d.report.Teardown.DaemonPIDs, command.Process.Pid)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(d.port))
	return d.wait(ctx, d.opts.Timeouts.DaemonReady, "the daemon to listen on "+address, func() (bool, error) {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return false, nil
		}
		connection.Close()
		return true, nil
	})
}

// stopDaemon sends the daemon SIGTERM and waits for it to exit; the panes it started keep running.
func (d *driver) stopDaemon() error {
	if d.daemon == nil || d.exited {
		return nil
	}
	if err := d.daemon.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case d.daemonErr = <-d.daemonDone:
		d.exited = true
		return nil
	case <-time.After(d.opts.Timeouts.DaemonStop):
		_ = d.daemon.Process.Kill()
		d.daemonErr = <-d.daemonDone
		d.exited = true
		return fmt.Errorf("the daemon did not exit within %s of SIGTERM and was killed", d.opts.Timeouts.DaemonStop)
	}
}

func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}

// wake enqueues each owner's start item, and checks that the supervisor starts both owner panes within the pane
// timeout, with the launch variables of C8 and the working directory its claim names (C9), and that each owner's
// session subscribes to its topics.
func (d *driver) wake(ctx context.Context) error {
	start := time.Now()
	for _, node := range d.nodes {
		item := "start:" + node + "@1"
		if err := d.store.Enqueue(ctx, d.run, item, node); err != nil {
			return err
		}
		d.expected.Outcomes = append(d.expected.Outcomes, Outcome{Item: item, Owner: node})
	}
	panes, seen, err := d.awaitPanes(ctx, d.panes, start)
	if err != nil {
		return fmt.Errorf("%w; the panes on tmux socket %s: %s", err, d.tmux.socket, d.tmux.paneCommands(ctx))
	}
	ownersDir := d.ownersDir()
	for _, node := range d.nodes {
		p := panes[node]
		d.report.Panes = append(d.report.Panes, PaneCheck{Owner: node, Pane: p.ID, PID: p.PID, StartSeconds: seen[node].Seconds(), Env: p.Env, Dir: p.Dir})
		want := map[string]string{"LEGION_OWNER": node, "LEGION_LEDGER_REPOSITORY": d.run, "LEGION_LEDGER_DSN_FILE": d.opts.Deploy.LedgerDSNFile}
		for _, name := range LaunchVariables {
			if got, ok := p.Env[name]; !ok || got != want[name] {
				d.fail(FailLaunch, node, "the pane's %s is %q (set %t), want %q", name, got, ok, want[name])
			}
		}
		if filepath.Dir(p.Dir) != ownersDir || !strings.HasSuffix(filepath.Base(p.Dir), EncodeOwnerName(node)) {
			d.fail(FailLaunch, node, "the pane works in %s, want %s/<claim token ending %s>", p.Dir, ownersDir, EncodeOwnerName(node))
		}
	}
	return d.awaitSubscriptions(ctx)
}

// awaitSubscriptions waits until each owner's session has subscribed to every topic the owner must: a notice reaches
// only the sessions subscribed when it is published, so the driver publishes after this. The evidence is the
// listener's record of the session's topics, which the session registers once it has opened its subscriptions, read
// for the session whose transcript the owner's pane holds open. The owner's own subscribed record is no evidence; the
// report shows it beside.
func (d *driver) awaitSubscriptions(ctx context.Context) error {
	client := &http.Client{Timeout: 10 * time.Second}
	dir := "legion-" + ProjectToken(daemonProject(d.run)) + "-owner-" // the claim token in each owner's working directory (C9)
	var read time.Time
	pending := "; the listener was not read yet"
	err := d.wait(ctx, d.opts.Timeouts.Subscribed, "each owner session to subscribe to its topics", func() (bool, error) {
		if _, err := d.panes(ctx); err != nil {
			return false, err
		}
		if time.Since(read) < listenerReadInterval {
			return false, nil
		}
		read = time.Now()
		registered, err := listenerTopics(ctx, client, d.listener, d.listenerToken, dir)
		if err != nil {
			pending = "; " + err.Error()
			return false, nil
		}
		pending = ""
		for index, node := range d.nodes {
			sessions := d.sessionIDs[node]
			check := &d.report.Panes[index]
			check.Sessions, check.Transcripts = sessions, d.transcriptDirs[node]
			if len(sessions) == 0 {
				pending += fmt.Sprintf("; %s: its pane holds no session transcript open", node)
				continue
			}
			topics := registered[sessions[len(sessions)-1]]
			check.Subscribed = slices.DeleteFunc(slices.Clone(topics), func(topic string) bool { return !strings.HasPrefix(topic, d.prefix) })
			if missing := slices.DeleteFunc(slices.Clone(d.subscribe[node]), func(topic string) bool { return slices.Contains(topics, topic) }); len(missing) > 0 {
				pending += fmt.Sprintf("; %s: the listener lists session %s without %v", node, sessions[len(sessions)-1], missing)
			}
		}
		return pending == "", nil
	})
	if errors.Is(err, errTimedOut) {
		d.fail(FailSubscription, strings.Join(d.nodes, ", "), "%s", strings.TrimPrefix(pending, "; "))
		return fmt.Errorf("%w%s", err, pending)
	}
	return err
}

// panes lists the owner panes and notes what their processes show: every owner process of the run, which the
// teardown stops, and the session transcripts they hold open, whose directory holds every transcript of that owner.
func (d *driver) panes(ctx context.Context) (map[string]pane, error) {
	panes, err := d.tmux.ownerPanes(ctx)
	if err != nil {
		return nil, err
	}
	for owner, p := range panes {
		d.watch(owner, p.Root)
	}
	return panes, nil
}

// ours reports whether pid is a process of the run: its environment names the run's ledger repository. A PID that a
// process of another run now holds, or one that exited, is not.
func (d *driver) ours(pid int) bool { return processEnv(pid)["LEGION_LEDGER_REPOSITORY"] == d.run }

// watch notes, in the process tree under root, each process of the run (its LEGION_LEDGER_REPOSITORY is the run's)
// and each session transcript a process holds open.
func (d *driver) watch(owner string, root int) {
	if d.ownerPIDs == nil {
		d.ownerPIDs, d.transcriptDirs, d.sessionIDs = map[int]string{}, map[string][]string{}, map[string][]string{}
	}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		pid := queue[0]
		if d.ours(pid) {
			d.ownerPIDs[pid] = owner
		}
		for _, transcript := range openTranscripts(pid) {
			if dir := filepath.Dir(transcript.Path); !slices.Contains(d.transcriptDirs[owner], dir) {
				d.transcriptDirs[owner] = append(d.transcriptDirs[owner], dir)
			}
			if ids := d.sessionIDs[owner]; len(ids) == 0 || ids[len(ids)-1] != transcript.SessionID {
				d.sessionIDs[owner] = append(ids, transcript.SessionID)
			}
		}
		queue = append(queue, children(pid)...)
	}
}

// ownersDir is the supervisor's directory of owner working directories, links resolved as /proc shows a working
// directory.
func (d *driver) ownersDir() string {
	dir, err := filepath.EvalSymlinks(filepath.Join(d.daemonState, "owners"))
	if err != nil {
		return filepath.Join(d.daemonState, "owners")
	}
	return dir
}

// awaitPanes waits, within the pane timeout from start, until list shows a pane for every node and each pane works
// in the supervisor's owners directory: a pane's shell starts in the tmux server's directory and changes into its
// owner directory before it starts the owner, so a pane seen earlier is still starting. seen is when each pane first
// appeared. When every pane appeared but one never reached the owners directory, the panes as last seen are
// returned with no error, for the launch check to judge.
func (d *driver) awaitPanes(ctx context.Context, list func(context.Context) (map[string]pane, error), start time.Time) (map[string]pane, map[string]time.Duration, error) {
	seen := map[string]time.Duration{}
	var panes map[string]pane
	err := d.wait(ctx, d.opts.Timeouts.PaneStart, "both owner panes", func() (bool, error) {
		var err error
		if panes, err = list(ctx); err != nil {
			return false, err
		}
		owners, started := d.ownersDir(), true
		for _, node := range d.nodes {
			p, ok := panes[node]
			if ok && seen[node] == 0 {
				seen[node] = time.Since(start)
			}
			started = started && ok && filepath.Dir(p.Dir) == owners
		}
		return started, nil
	})
	if errors.Is(err, errTimedOut) && len(seen) == len(d.nodes) {
		return panes, seen, nil
	}
	return panes, seen, err
}

// arrivals reads every delivery that each owner session's transcripts record, by owner and event id. A fork of a
// session starts with a copy of its parent's entries, each under the entry id it had there; a copy is one delivery,
// not another, so each owner's entry ids count once.
func (d *driver) arrivals() (map[string]map[string][]Arrival, error) {
	out := map[string]map[string][]Arrival{}
	for _, node := range d.nodes {
		out[node] = map[string][]Arrival{}
		entries := map[string]bool{}
		for _, dir := range d.transcriptDirs[node] {
			transcripts, err := sessionTranscripts(dir)
			if err != nil {
				return nil, err
			}
			for _, transcript := range transcripts {
				arrivals, err := readArrivals(node, transcript.Path)
				if err != nil {
					return nil, err
				}
				for _, arrival := range arrivals {
					if arrival.Entry != "" && entries[arrival.Entry] {
						continue
					}
					entries[arrival.Entry] = true
					out[node][arrival.ID] = append(out[node][arrival.ID], arrival)
				}
			}
		}
	}
	return out, nil
}

// arrived reports whether the session of every receiver of each publication recorded it.
func (d *driver) arrived(publications ...Publication) (bool, error) {
	arrivals, err := d.arrivals()
	if err != nil {
		return false, err
	}
	for _, publication := range publications {
		for _, receiver := range publication.Receivers {
			if len(arrivals[receiver][publication.ID]) == 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

// receivers are the owners that must subscribe to topic.
func (d *driver) receivers(topic string) []string {
	var out []string
	for _, node := range d.nodes {
		if slices.Contains(d.subscribe[node], topic) {
			out = append(out, node)
		}
	}
	return out
}

// publish publishes one notice under its own idempotency key: every publication of the run is a distinct Envoy
// message, so a notice published twice is delivered twice, as Envoy's at-least-once delivery may.
func (d *driver) publish(ctx context.Context, step, kind, topic, item string) (Publication, error) {
	key := "mt" + ownerpkg.Hex16(d.run+"\x00"+step+"\x00"+topic+"\x00"+item+"\x00"+strconv.Itoa(len(d.report.Published)))
	payload := map[string]string{"type": "message-test", "run": d.run, "step": step, "topic": topic}
	if item != "" {
		payload["item"] = item
	}
	at := time.Now().UTC()
	id, err := d.publisher.PublishEvent(ctx, topic, fmt.Sprintf("message test %s: a %s notice on %s", step, kind, topic), payload, key)
	if err != nil {
		return Publication{}, err
	}
	publication := Publication{Step: step, Kind: kind, Topic: topic, ID: id, Key: key, At: at, Receivers: d.receivers(topic)}
	d.report.Published = append(d.report.Published, publication)
	return publication, nil
}

// pickArrivalTopics picks the topic of each kind that the arrival step publishes on. A kind that no topic of the two
// nodes has stops the run, before any daemon or owner session runs, unless the operator allowed it
// (--allow-missing-kinds); the report lists each allowed kind the nodes lack.
func (d *driver) pickArrivalTopics() error {
	var held []string
	for _, node := range d.nodes {
		held = append(held, d.topics.Owners[node]...)
	}
	picks, missing := arrivalTopics(held)
	var refused []string
	for _, kind := range missing {
		if slices.Contains(d.opts.AllowMissingKinds, kind) {
			d.report.AllowedMissingKinds = append(d.report.AllowedMissingKinds, kind)
		} else {
			refused = append(refused, kind)
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("the nodes %s hold no topic of the kinds %s, so the arrival step cannot show a delivery of them; "+
			"choose nodes that hold them, or pass --allow-missing-kinds %s to run without them", strings.Join(d.nodes, ", "),
			strings.Join(refused, ", "), strings.Join(refused, ","))
	}
	d.arrivalPicks = picks
	return nil
}

// arrivalTopic is the topic the arrival step publishes one notice on for a topic kind.
type arrivalTopic struct{ kind, topic string }

// arrivalTopics picks, for each of TopicKinds, the first topic of held in sort order with that kind, and lists the
// kinds that no topic of held has.
func arrivalTopics(held []string) (picks []arrivalTopic, missing []string) {
	held = slices.Compact(slices.Sorted(slices.Values(held)))
	for _, kind := range TopicKinds {
		index := slices.IndexFunc(held, func(topic string) bool { return topicKind(topic) == kind })
		if index < 0 {
			missing = append(missing, kind)
			continue
		}
		picks = append(picks, arrivalTopic{kind, held[index]})
	}
	return picks, missing
}

// arrival publishes one notice on each topic kind the two owners hold, then on the fact and concept topics the
// source owner subscribed to, and waits until the session of every receiver recorded each of them.
func (d *driver) arrival(ctx context.Context) error {
	notices := slices.Clone(d.arrivalPicks)
	for _, topic := range []string{d.prefix + "fact." + testFact + ".users", d.prefix + "fact." + testFact + ".needs", d.prefix + "concepts"} {
		notices = append(notices, arrivalTopic{strings.TrimPrefix(topic, d.prefix), topic})
	}
	var publications []Publication
	for _, n := range notices {
		publication, err := d.publish(ctx, "arrival", n.kind, n.topic, "")
		if err != nil {
			return err
		}
		if len(publication.Receivers) == 0 {
			d.fail(FailStep, "arrival", "no owner subscribes to %s", n.topic)
		}
		for _, receiver := range publication.Receivers {
			d.expected.Deliveries = append(d.expected.Deliveries, ExpectedDelivery{Owner: receiver, Topic: n.topic, ID: publication.ID, Required: true})
		}
		publications = append(publications, publication)
	}
	return d.wait(ctx, d.opts.Timeouts.Delivery, "every arrival notice to reach its receivers' sessions", func() (bool, error) {
		return d.arrived(publications...)
	})
}

func (d *driver) need(version string) string {
	return "need:" + ownerpkg.EdgeID(d.source, d.target) + "@" + version
}

// offer is the offer item that answering the need of version queues: the run expects it (expectAnswered) before it
// waits for the answer.
func (d *driver) offer(version string) string { return d.offers[version] }

// expectAnswered records that the need of version, which step enqueued, must be answered once, with its follow-up
// offer once, and that the target's answer publishes the offer's notice itself, which must reach the source's session:
// on the topic of the routing table's offer row for the target (a data structure's users topic, a function's callers
// topic), from the target, naming the offer. The offer is numbered by production's rule, by the revision of the
// record that answers the need: the target answers the run's needs one at a time, each rewriting its answer record as
// the next revision, so the n-th need it answers queues offer:<edge>@r<n>, whatever the need's own version.
func (d *driver) expectAnswered(step, version string) {
	if d.offers == nil {
		d.offers = map[string]string{}
	}
	offer := fmt.Sprintf("offer:%s@r%d", ownerpkg.EdgeID(d.source, d.target), len(d.offers)+1)
	d.offers[version] = offer
	d.expected.Outcomes = append(d.expected.Outcomes,
		Outcome{Item: d.need(version), Owner: d.target, FollowUp: offer, FollowUpOwner: d.source},
		Outcome{Item: offer, Owner: d.source, Optional: true})
	for _, delivery := range d.routing.Topics(ownerpkg.Route{Step: ownerpkg.RouteOffer, Source: d.source, Target: d.target, TargetKind: d.targetKind}) {
		d.expected.OwnerNotices = append(d.expected.OwnerNotices,
			ExpectedOwnerNotice{Step: step, Receiver: delivery.Receiver, From: d.target, Type: "offer", Item: offer, Topic: delivery.Topic})
	}
}

// enqueueNeed enqueues the need of version for the target and publishes its notice on the target's needs topic.
// required says whether the target's session must record the notice: it was running and subscribed, and the run
// does not kill it before it can.
func (d *driver) enqueueNeed(ctx context.Context, step, version string, required bool) (Publication, error) {
	item := d.need(version)
	if err := d.store.Enqueue(ctx, d.run, item, d.target); err != nil {
		return Publication{}, err
	}
	return d.notifyTarget(ctx, step, item, required)
}

// notifyTarget publishes a notice about item on the target's needs topic and records what its delivery must be.
func (d *driver) notifyTarget(ctx context.Context, step, item string, required bool) (Publication, error) {
	publication, err := d.publish(ctx, step, "needs", d.routing.NeedsTopic(d.target), item)
	if err != nil {
		return Publication{}, err
	}
	d.expected.Deliveries = append(d.expected.Deliveries, ExpectedDelivery{Owner: d.target, Topic: publication.Topic, ID: publication.ID, Required: required})
	return publication, nil
}

func (d *driver) itemDone(ctx context.Context, item string) (bool, error) {
	current, err := d.store.Item(ctx, d.run, item)
	var notFound *ledger.NotFoundError
	if errors.As(err, &notFound) {
		return false, nil
	}
	return err == nil && current.State == "done", err
}

func (d *driver) itemExists(ctx context.Context, item string) (bool, error) {
	_, err := d.store.Item(ctx, d.run, item)
	var notFound *ledger.NotFoundError
	if errors.As(err, &notFound) {
		return false, nil
	}
	return err == nil, err
}

// answered reports whether the need of version is done, its follow-up offer exists, and the target's own notice of
// that offer reached the source's session.
func (d *driver) answered(ctx context.Context, version string) (bool, error) {
	done, err := d.itemDone(ctx, d.need(version))
	if err != nil || !done {
		return false, err
	}
	if exists, err := d.itemExists(ctx, d.offer(version)); err != nil || !exists {
		return false, err
	}
	return d.noticed(d.offer(version))
}

// noticed reports whether each owner notice of item reached its receiver's session on its topic.
func (d *driver) noticed(item string) (bool, error) {
	arrivals, err := d.arrivals()
	if err != nil {
		return false, err
	}
	for _, notice := range d.expected.OwnerNotices {
		if notice.Item != item {
			continue
		}
		var received []Arrival
		for _, list := range arrivals[notice.Receiver] {
			received = append(received, list...)
		}
		if onTopic, _ := ownerNoticeArrivals(notice, received); len(onTopic) == 0 {
			return false, nil
		}
	}
	return true, nil
}

// duplicates enqueues the need r1 twice, and publishes its notice twice under two idempotency keys, as Envoy may
// deliver one message twice: both deliveries must reach the owner's session and the need be answered once, and the
// target's own notice of the offer its answer queues must reach the source's session.
func (d *driver) duplicates(ctx context.Context) error {
	item := d.need("r1")
	if err := enqueueTwice(ctx, d.store, d.run, item, d.target); err != nil {
		return err
	}
	var copies []Publication
	for range 2 {
		publication, err := d.notifyTarget(ctx, "duplicates", item, true)
		if err != nil {
			return err
		}
		copies = append(copies, publication)
	}
	d.expectAnswered("duplicates", "r1")
	return d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered, both of its notices to reach the target's session and the offer's notice the source's", func() (bool, error) {
		done, err := d.answered(ctx, "r1")
		if err != nil || !done {
			return false, err
		}
		return d.arrived(copies...)
	})
}

// queue is the part of the ledger that a repeated enqueue uses.
type queue interface {
	Enqueue(ctx context.Context, repo, id, owner string) error
	Item(ctx context.Context, repo, id string) (ledger.Item, error)
}

// enqueueTwice enqueues item for owner twice, as a duplicated work item reaches the ledger. The first enqueue makes
// the item, and the owner can answer it before the second: the ledger then refuses the repeat as an existing item,
// which is as much a duplicate as the idempotent repeat of an open item, so it is no failure. The run's check still
// requires one record and one completion. Any other outcome of either enqueue is an error.
func enqueueTwice(ctx context.Context, q queue, repo, item, owner string) error {
	if err := q.Enqueue(ctx, repo, item, owner); err != nil {
		return err
	}
	err := q.Enqueue(ctx, repo, item, owner)
	var refusal *ledger.Refusal
	if err == nil || !errors.As(err, &refusal) || refusal.Reason != ledger.ReasonExists {
		return err
	}
	current, itemErr := q.Item(ctx, repo, item)
	if itemErr != nil {
		return errors.Join(err, itemErr)
	}
	if current.Owner == owner && current.State == "done" {
		return nil
	}
	return err
}

// outOfOrder enqueues r3 and then r2 with their notices: r3 must be answered, r2 completed as superseded:r3 with no
// record, both notices must reach the running owner's session, and the target's own notice of the offer r3's answer
// queues must reach the source's. Each condition is the ledger's or a session transcript's, never the owner model's
// record of a notice: the model may handle a burst of deliveries and end its turn without recording one.
func (d *driver) outOfOrder(ctx context.Context) error {
	var notices []Publication
	for _, version := range []string{"r3", "r2"} {
		publication, err := d.enqueueNeed(ctx, "out of order", version, true)
		if err != nil {
			return err
		}
		notices = append(notices, publication)
	}
	d.expectAnswered("out of order", "r3")
	d.expected.Outcomes = append(d.expected.Outcomes, Outcome{Item: d.need("r2"), Owner: d.target, Superseded: "r3"})
	return d.wait(ctx, d.opts.Timeouts.Item, "r3 to be answered, r2 to be done and both notices to reach the session", func() (bool, error) {
		done, err := d.answered(ctx, "r3")
		if err != nil || !done {
			return false, err
		}
		if done, err = d.itemDone(ctx, d.need("r2")); err != nil || !done {
			return false, err
		}
		return d.arrived(notices...)
	})
}

// itemAt is the target's item as it stands: its holder, the commit of its record on the target's branch (the commit
// whose Owner-Item trailer names it), and whether it is done.
func (d *driver) itemAt(ctx context.Context, item string) (holder, record string, done bool, err error) {
	current, err := d.store.Item(ctx, d.run, item)
	if err != nil {
		return "", "", false, err
	}
	commits, err := branchCommits(ctx, d.bare, d.target)
	if err != nil {
		return "", "", false, err
	}
	for _, commit := range commits {
		if slices.Contains(commit.Trailers[itemTrailer], item) {
			record = commit.SHA
		}
	}
	return current.Holder, record, current.State == "done", nil
}

// kill enqueues a new need and kills the target's pane at the moment the schedule sets for this kill. Before the
// claim, it kills the running pane as soon as the need is open and unclaimed. After the claim and after the answer,
// it holds the owner's answer at that moment (PutHold: claimed with nothing recorded, or recorded and not yet
// completed) and kills the pane once the answer says it is held. The pane's processes must all be gone before the
// hold is released, so no process of the killed pane goes on with the step. The supervisor must start the owner
// again while the item is open, and the item must still be answered once. The need's notice is optional: the kill can
// end the session before it records it, and the ledger item is the wake that lasts.
func (d *driver) kill(index int) func(context.Context) error {
	return func(ctx context.Context) error {
		d.killsStarted = true
		moment := d.report.KillSchedule[index]
		version := fmt.Sprintf("r%d", 4+index)
		item := d.need(version)
		kill := Kill{Item: item, Scheduled: moment}
		defer func() { d.report.KillLog = append(d.report.KillLog, kill) }()
		if moment != MomentBeforeClaim {
			if err := PutHold(d.stateDir, item, moment); err != nil {
				return err
			}
			// A step that ends early must not leave the owner held for ever.
			defer func() { _ = ReleaseHold(d.stateDir) }()
		}
		if _, err := d.enqueueNeed(ctx, "kill", version, false); err != nil {
			return err
		}
		d.expectAnswered("kill", version)
		var target pane
		err := d.wait(ctx, d.opts.Timeouts.Item, "the "+moment+" moment of "+item, func() (bool, error) {
			panes, err := d.panes(ctx)
			if err != nil {
				return false, err
			}
			holder, record, done, err := d.itemAt(ctx, item)
			if err != nil {
				return false, err
			}
			if done {
				return false, fmt.Errorf("%s was answered before the %s kill: the owner did not pause at the hold", item, moment)
			}
			p, live := panes[d.target]
			if !live {
				return false, nil
			}
			switch moment {
			case MomentBeforeClaim:
				if holder != "" || record != "" {
					return false, fmt.Errorf("%s was claimed (by %q, record %q) before the driver could kill its owner's pane before the claim", item, holder, record)
				}
			default:
				held, found, err := ReadHeld(d.stateDir)
				if err != nil || !found || held.Item != item || held.Moment != moment {
					return false, err
				}
				kill.Held = held.PID
			}
			target = p
			return true, nil
		})
		if err != nil {
			return err
		}
		kill.Pane, kill.PID, kill.At = target.ID, target.PID, time.Now().UTC()
		if kill.Holder, kill.Record, kill.Done, err = d.killObserving(ctx, target, item); err != nil {
			return err
		}
		kill.Observed = observedMoment(d.target, kill.Holder, kill.Record, kill.Done)
		if moment != MomentBeforeClaim {
			if err := ReleaseHold(d.stateDir); err != nil {
				return err
			}
		}
		d.logf("killed %s of %s %s (scheduled %s; holder %q, record %q, held owner command %d)", kill.Pane, d.target, kill.Observed, moment, kill.Holder, kill.Record, kill.Held)
		return d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered after the kill", func() (bool, error) {
			if kill.RelaunchSeconds == 0 {
				panes, err := d.panes(ctx)
				if err != nil {
					return false, err
				}
				// A pane id can come back when the killed pane was the server's last, so a relaunch is a new process.
				if p, live := panes[d.target]; live && p.Root != target.Root {
					kill.RelaunchSeconds = time.Since(kill.At).Seconds()
				}
			}
			return d.answered(ctx, version)
		})
	}
}

// killObserving kills the target's pane p and every process of it, and reads item as the kill found it: its holder,
// its record commit and whether it is done. The read comes before the kill, while every process of the pane is
// stopped (SIGSTOP) and still alive: the supervisor frees a dead owner's leases as soon as it sees its pane gone (the
// daemon's "supervise: process died", the harness supervisor's probe), so a read after the kill can find the holder
// gone, and stopped processes can change nothing while the item is read. A commit the owner sent before it stopped
// lands within the poll interval the read waits first.
func (d *driver) killObserving(ctx context.Context, p pane, item string) (holder, record string, done bool, err error) {
	d.stopPane(p)
	select {
	case <-ctx.Done():
	case <-time.After(d.opts.Timeouts.Poll):
	}
	holder, record, done, err = d.itemAt(ctx, item)
	return holder, record, done, errors.Join(err, d.killPaneTree(ctx, p))
}

// stopPane stops (SIGSTOP) every process of the run under the pane's root, walking the tree again until a walk finds
// none it has not stopped, so a process forked during a walk is stopped too.
func (d *driver) stopPane(p pane) {
	stopped := map[int]bool{}
	for added := true; added; {
		added = false
		for queue := []int{p.Root}; len(queue) > 0; queue = queue[1:] {
			pid := queue[0]
			if !stopped[pid] && d.ours(pid) {
				_ = syscall.Kill(pid, syscall.SIGSTOP)
				stopped[pid], added = true, true
			}
			queue = append(queue, children(pid)...)
		}
	}
}

// killPaneTree kills an owner pane and every process under it that names the run, and waits until they are gone: a
// pane's SIGHUP does not reach a command its session started in a process group of its own, and such a command
// (a held answer) must not go on with the step once the pane is killed.
func (d *driver) killPaneTree(ctx context.Context, p pane) error {
	var tree []int
	for queue := []int{p.Root}; len(queue) > 0; queue = queue[1:] {
		if d.ours(queue[0]) {
			tree = append(tree, queue[0])
		}
		queue = append(queue, children(queue[0])...)
	}
	if err := d.tmux.killPane(ctx, p.ID); err != nil {
		if _, alive, _ := ProcessIncarnation(p.Root); alive {
			return err
		}
	}
	for _, pid := range tree {
		if d.ours(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	deadline := time.Now().Add(max(d.opts.Timeouts.DaemonStop, time.Second))
	for {
		left := slices.DeleteFunc(slices.Clone(tree), func(pid int) bool { return !d.ours(pid) })
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the processes %v of the killed pane %s still run", left, p.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.opts.Timeouts.Poll):
		}
	}
}

// paneProcess is p as the report names a pane: its id, its root process and that process's incarnation.
func paneProcess(p pane) (PaneProcess, error) {
	incarnation, alive, err := ProcessIncarnation(p.Root)
	if err == nil && !alive {
		err = fmt.Errorf("the pane %s's process %d is gone", p.ID, p.Root)
	}
	return PaneProcess{Pane: p.ID, PID: p.Root, Incarnation: incarnation}, err
}

// restart stops the daemon while the target's pane runs, starts it again, kills that pane and enqueues a new need, so
// that only a pane the new daemon launches can answer it. The step passes when the new daemon did the work
// (checkRestart): the need claimed and completed by the target after the restart, with every target pane seen while
// it was open one the new daemon logged as launched (the daemon's log after the driver started it, which only the new
// daemon writes). The need's own notice is optional (no pane runs when it is published); after the need is answered,
// a second notice must reach the session of the pane the new daemon launched.
func (d *driver) restart(ctx context.Context) error {
	version := fmt.Sprintf("r%d", 4+d.opts.Kills)
	item := d.need(version)
	restart := &Restart{Item: item}
	d.report.Restart = restart
	var old pane
	if err := d.wait(ctx, d.opts.Timeouts.PaneStart, "the pane of "+d.target, func() (bool, error) {
		panes, err := d.panes(ctx)
		if err != nil {
			return false, err
		}
		p, live := panes[d.target]
		old = p
		return live, nil
	}); err != nil {
		return err
	}
	var err error
	if restart.OldPane, err = paneProcess(old); err != nil {
		return err
	}
	restart.OldDaemonPID = d.daemon.Process.Pid
	start := time.Now()
	if err := d.stopDaemon(); err != nil {
		return err
	}
	restart.StopSeconds = time.Since(start).Seconds()
	info, err := os.Stat(d.daemonLog)
	if err != nil {
		return err
	}
	offset := info.Size()
	started := time.Now()
	if err := d.startDaemon(ctx); err != nil {
		return err
	}
	restart.StartSeconds, restart.NewDaemonPID = time.Since(started).Seconds(), d.daemon.Process.Pid
	// The old daemon's pane would otherwise answer the need itself.
	if err := d.killPaneTree(ctx, old); err != nil {
		return fmt.Errorf("kill the old daemon's pane %s of %s: %w", old.ID, d.target, err)
	}
	if restart.EventsFrom, err = d.store.LatestEventID(ctx, d.run); err != nil {
		return err
	}
	if _, err := d.enqueueNeed(ctx, "restart", version, false); err != nil {
		return err
	}
	d.expectAnswered("restart", version)
	seen := func() error {
		panes, err := d.panes(ctx)
		if err != nil {
			return err
		}
		p, live := panes[d.target]
		if !live || slices.ContainsFunc(restart.WorkPanes, func(w PaneProcess) bool { return w.PID == p.Root }) {
			return nil
		}
		if work, err := paneProcess(p); err == nil {
			restart.WorkPanes = append(restart.WorkPanes, work)
		}
		return nil
	}
	err = d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered after the restart", func() (bool, error) {
		if err := seen(); err != nil {
			return false, err
		}
		return d.answered(ctx, version)
	})
	restart.ItemSeconds = time.Since(start).Seconds()
	if err != nil {
		return err
	}
	if err := seen(); err != nil {
		return err
	}
	if restart.Launched, err = launchedIncarnations(d.daemonLog, offset, d.target); err != nil {
		return err
	}
	events, err := d.store.Events(ctx, d.run, restart.EventsFrom, item)
	if err != nil {
		return err
	}
	for _, event := range events {
		switch event.Kind {
		case "item.claimed":
			restart.ClaimEvent = event.ID
		case "item.completed":
			restart.CompleteEvent = event.ID
		}
	}
	d.failures = append(d.failures, checkRestart(*restart, events, d.target)...)
	after, err := d.notifyTarget(ctx, "restart", item, true)
	if err != nil {
		return err
	}
	return d.wait(ctx, d.opts.Timeouts.Delivery, "the restart's notice to reach the new pane's session", func() (bool, error) {
		return d.arrived(after)
	})
}

// collect reads the run's ledger events, the owners' branches and every delivery the owners' session transcripts
// record, and reports each expected delivery: whether and where the session recorded it, and whether the owner model
// also recorded it with `owner message-test saw`, a second signal that decides nothing; and each owner notice:
// whether, where and how often its receiver's session recorded it on its topic.
func (d *driver) collect(ctx context.Context) (Recorded, error) {
	var recorded Recorded
	if d.store == nil {
		return recorded, nil
	}
	events, err := d.store.Events(ctx, d.run, 0, "")
	if err != nil {
		return recorded, err
	}
	recorded.Events = events
	for _, node := range d.nodes {
		commits, err := branchCommits(ctx, d.bare, node)
		if err != nil {
			return recorded, err
		}
		recorded.Commits = append(recorded.Commits, commits...)
	}
	arrivals, err := d.arrivals()
	if err != nil {
		return recorded, err
	}
	for _, node := range d.nodes {
		for _, id := range slices.Sorted(maps.Keys(arrivals[node])) {
			recorded.Arrivals = append(recorded.Arrivals, arrivals[node][id]...)
		}
	}
	for index, node := range d.nodes {
		var record Subscribed
		found, err := readRecord(ctx, d.bare, node, SubscribedRecord(d.root, node), &record)
		if err != nil {
			return recorded, err
		}
		if found && index < len(d.report.Panes) {
			d.report.Panes[index].Topics = record.Topics
		}
	}
	published := map[string]Publication{}
	for _, publication := range d.report.Published {
		published[publication.ID] = publication
	}
	for _, expected := range d.expected.Deliveries {
		delivery := Delivery{Owner: expected.Owner, ID: expected.ID, Topic: expected.Topic, Required: expected.Required}
		at := published[expected.ID].At
		if arrived := arrivals[expected.Owner][expected.ID]; len(arrived) > 0 {
			delivery.Arrived, delivery.Transcript, delivery.Line = true, arrived[0].Transcript, arrived[0].Line
			delivery.ArrivalSeconds = arrived[0].At.Sub(at).Seconds()
		}
		var saw Saw
		found, err := readRecord(ctx, d.bare, expected.Owner, SawRecord(d.root, expected.Owner, expected.ID), &saw)
		if err != nil {
			return recorded, err
		}
		if found {
			delivery.Saw, delivery.SawSeconds = true, saw.ReceivedAt.Sub(at).Seconds()
		}
		d.report.Deliveries = append(d.report.Deliveries, delivery)
	}
	for _, notice := range d.expected.OwnerNotices {
		onTopic, _ := ownerNoticeArrivals(notice, recorded.Arrivals)
		report := OwnerNotice{ExpectedOwnerNotice: notice, Arrivals: len(onTopic)}
		if len(onTopic) > 0 {
			first := slices.MinFunc(onTopic, func(a, b Arrival) int { return a.At.Compare(b.At) })
			report.Arrived, report.EventID, report.DedupeKey, report.Transcript, report.Line, report.At = true, first.ID, first.DedupeKey, first.Transcript, first.Line, first.At
		}
		d.report.OwnerNotices = append(d.report.OwnerNotices, report)
	}
	return recorded, nil
}

// teardown stops what the run started and removes the token files: the daemon by its own PID, every pane by the
// run's own tmux socket, then every owner process the run saw that still runs, by its own PID; then the bearer files
// the run wrote, the daemon's boot tokens and the run's tmux directory, which holds the socket. It runs on a context
// of its own, bounded by cleanupTimeout, so it also stops a run whose context ended (an interrupt).
func (d *driver) teardown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var errs []error
	if err := d.stopDaemon(); err != nil {
		errs = append(errs, err)
	}
	// The run's tmux server, once setup made its directory. tmux never removes a socket file, so the directory goes
	// once the server is stopped; while the server may still run, its socket stays, so that it can be reached.
	if _, err := os.Stat(d.tmux.tmpDir); err == nil {
		// The panes' processes as they are now, so that the owners stopped below include the newest.
		if _, err := d.panes(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := d.tmux.killServer(ctx); err != nil {
			errs = append(errs, err)
		} else if err := os.RemoveAll(d.tmux.tmpDir); err != nil {
			errs = append(errs, err)
		} else {
			d.report.Teardown.TmuxDir = d.tmux.tmpDir
		}
	}
	if err := d.stopOwners(ctx); err != nil {
		errs = append(errs, err)
	}
	if d.tokenDir != "" {
		d.report.Teardown.TokenDir = d.tokenDir
		if err := os.RemoveAll(d.tokenDir); err != nil {
			errs = append(errs, err)
		}
	}
	// The daemon writes each pane's boot token under its state_dir (<state_dir>/secrets/<claim token>), which is in
	// the run's directory. With the daemon and every pane stopped, nothing reads them again.
	if d.daemonState != "" {
		secrets := filepath.Join(d.daemonState, "secrets")
		d.report.Teardown.DaemonSecrets = secrets
		if err := os.RemoveAll(secrets); err != nil {
			errs = append(errs, err)
		}
	}
	if d.store != nil {
		d.store.Close()
	}
	return errors.Join(errs...)
}

// stopOwners stops each owner process the run saw that still runs, by its own PID: SIGTERM, then SIGKILL to any left
// after the daemon's stop timeout. A process is the run's only while its environment names the run's ledger
// repository, so a PID that now names another process is left alone.
func (d *driver) stopOwners(ctx context.Context) error {
	pids := slices.Sorted(maps.Keys(d.ownerPIDs))
	d.report.Teardown.OwnerPIDs = pids
	running := func() []int {
		return slices.DeleteFunc(slices.Clone(pids), func(pid int) bool { return !d.ours(pid) })
	}
	// awaitExit waits, up to the daemon's stop timeout, until none of the run's owner processes runs, and returns
	// those that still do.
	awaitExit := func() []int {
		deadline := time.Now().Add(max(d.opts.Timeouts.DaemonStop, time.Second))
		left := running()
		for len(left) > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(d.opts.Timeouts.Poll)
			left = running()
		}
		return left
	}
	for _, pid := range running() {
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			d.report.Teardown.Stopped = append(d.report.Teardown.Stopped, pid)
		}
	}
	left := awaitExit()
	if len(left) > 0 {
		for _, pid := range left {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		left = awaitExit()
	}
	if len(left) > 0 {
		d.report.Teardown.Left = left
		return fmt.Errorf("the run's owner processes %v still run after SIGTERM and SIGKILL", left)
	}
	return nil
}
