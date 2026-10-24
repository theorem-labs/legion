// Package messagetest is the owner message test (plan Task 1.8): before any production owner starts, it shows
// that a notice on each topic kind reaches real owner sessions that the supervisor starts from their ledger queues,
// and that duplicate notices and items, items out of order, killed owner sessions and a supervisor restart still
// leave each step done once. The driver reads every assertion from durable state: the ledger's events and the
// owners' branches.
package messagetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
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
	Prefix string   // the topic prefix, under notifications. and ending in a dot; empty means the graph's own
	Nodes  []string // the two owners; none means pickNodes chooses them from the graph
	Kills  int      // how many owner panes to kill
	Out    string   // the run's directory, absolute and new or empty

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

var moments = []string{MomentBeforeClaim, MomentAfterClaim, MomentAfterAnswer}

func momentRank(moment string) int { return slices.Index(moments, moment) }

type driver struct {
	opts   Options
	report *Report
	rng    *rand.Rand

	run            string // the ledger repository and the daemon's project
	root           string // the copy's records root
	nodes          []string
	source, target string
	bare           string
	stateDir       string // the test deployment's state_dir
	deployPath     string
	daemonState    string
	daemonConfig   string
	daemonLog      string
	socket         string
	port           int
	tokenDir       string // the private directory of the bearer files the run wrote, removed at teardown
	omp            string // the omp the owner panes run, which the supervisor gets as LEGION_OMP_PATH

	store     *ledger.Store
	publisher *ownerpkg.HTTPPublisher
	routing   ownerpkg.Routing
	topics    ownerpkg.Topics     // the test graph's topics.json
	subscribe map[string][]string // the topics each owner's subscribed record must list
	expected  Expected
	offers    map[string]string // need version → the offer its answer queues (expectAnswered)
	failures  []Failure

	daemon     *exec.Cmd
	daemonDone chan error
	daemonErr  error
	exited     bool
}

// Run runs the message test and writes DIR/report.json and DIR/report.md. The error is only for options it cannot
// start with; every failure of the run is in the report, which passes only when every assertion held.
func Run(ctx context.Context, opts Options) (Report, error) {
	if err := opts.check(); err != nil {
		return Report{}, err
	}
	if err := makeOut(opts.Out); err != nil {
		return Report{}, err
	}
	now := time.Now().UTC()
	seed := now.UnixNano()
	d := &driver{
		opts: opts, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed)>>1)),
		run: "message-test-" + strings.ToLower(now.Format("20060102T150405Z")), root: "skeleton",
		report: &Report{Prefix: opts.Prefix, Out: opts.Out, Seed: seed, StartedAt: now},
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
		steps = append(steps, step{fmt.Sprintf("kill %d of %d", i+1, opts.Kills), d.kill(i), false})
	}
	steps = append(steps, step{"daemon restart", d.restart, false})
	for _, s := range steps {
		if err := d.timed(ctx, s.name, s.run); err != nil && s.fatal {
			break
		}
	}
	var recorded Recorded
	_ = d.timed(ctx, "collect", func(ctx context.Context) error {
		var err error
		recorded, err = d.collect(ctx)
		return err
	})
	_ = d.timed(ctx, "teardown", d.teardown)
	failures := append(d.failures, Check(d.expected, recorded)...)
	d.report.Expected = d.expected
	d.report.FinishedAt = time.Now().UTC()
	d.report.Finish(failures)
	if err := d.report.Write(opts.Out); err != nil {
		return *d.report, err
	}
	d.logf("report %s: passed %t, %d failures", filepath.Join(opts.Out, "report.md"), d.report.Passed, len(failures))
	return *d.report, nil
}

func (o Options) check() error {
	switch {
	case o.Prefix != "" && !ownerpkg.ValidTopicPrefix(o.Prefix):
		return fmt.Errorf("prefix %q is not a prefix under notifications. that ends in a dot", o.Prefix)
	case len(o.Nodes) != 0 && (len(o.Nodes) != 2 || o.Nodes[0] == o.Nodes[1]):
		return fmt.Errorf("the message test takes two different nodes, not %v", o.Nodes)
	case o.Kills < 0:
		return fmt.Errorf("kills %d is negative", o.Kills)
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
	d.report.Edge = ownerpkg.EdgeID(d.source, d.target)
	if d.opts.Prefix == "" {
		// No prefix given: the run keeps the one the deployment's graph names, which its owners use.
		d.opts.Prefix = graph.topics.Prefix
	}
	d.report.Prefix = d.opts.Prefix

	graphDir := filepath.Join(out, "graph")
	topics, owners, err := graph.write(graphDir, d.opts.Prefix, d.nodes, d.source)
	if err != nil {
		return err
	}
	d.topics = topics
	d.bare = filepath.Join(out, "repo.git")
	if err := createCopy(d.bare, filepath.Join(out, "seed"), copyConfig(d.run, d.opts.Prefix, graphDir), d.nodes); err != nil {
		return err
	}
	factOwners := ownerpkg.FactOwners{"fact-" + testFact: testFact}
	if d.routing, err = ownerpkg.NewRouting(d.opts.Prefix, factOwners); err != nil {
		return err
	}

	// The source owner also subscribes to the fact owner's and the librarian's topics, which no node owner holds.
	d.stateDir = filepath.Join(out, "state")
	extra := map[string][]string{d.source: {d.opts.Prefix + "fact." + testFact + ".needs", d.opts.Prefix + "concepts"}}
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
	d.publisher = ownerpkg.NewHTTPPublisher(url, token)
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
	d.socket = DaemonSocket(d.run)
	supervisor := daemonConfig(d.run, d.daemonState, d.port, url, natsURLs, tokenFile, operatorToken, d.opts.Deploy.LedgerDSNFile, d.run, prompt, kinds)
	if data, err = yaml.Marshal(supervisor); err != nil {
		return err
	}
	if err := os.WriteFile(d.daemonConfig, data, 0o644); err != nil {
		return err
	}
	d.report.Daemon = Daemon{Executable: d.opts.Legion, Config: d.daemonConfig, Socket: d.socket, Log: d.daemonLog, Port: d.port, Omp: d.omp}
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
// panes run, and no launch variable of the driver's own, so that a pane's launch variables are the supervisor's.
func (d *driver) daemonEnv() []string {
	env := slices.DeleteFunc(slices.Clone(d.opts.Environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == "PATH" || name == "LEGION_OMP_PATH" || slices.Contains(LaunchVariables, name)
	})
	return append(env, "PATH="+d.daemonPath(), "LEGION_OMP_PATH="+d.omp)
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
// timeout, with the launch variables of C8 and the working directory its claim names (C9), and that each owner
// records the topics it subscribed to.
func (d *driver) wake(ctx context.Context) error {
	start := time.Now()
	for _, node := range d.nodes {
		item := "start:" + node + "@1"
		if err := d.store.Enqueue(ctx, d.run, item, node); err != nil {
			return err
		}
		d.expected.Outcomes = append(d.expected.Outcomes, Outcome{Item: item, Owner: node})
	}
	panes, seen, err := d.awaitPanes(ctx, ownerPanes, start)
	if err != nil {
		return fmt.Errorf("%w; the panes on tmux socket %s: %s", err, d.socket, paneCommands(ctx, d.socket))
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
	return d.wait(ctx, d.opts.Timeouts.Subscribed, "both subscribed records", func() (bool, error) {
		for index, node := range d.nodes {
			if d.report.Panes[index].Topics != nil {
				continue
			}
			var record Subscribed
			found, err := readRecord(ctx, d.bare, node, SubscribedRecord(d.root, node), &record)
			if err != nil || !found {
				return false, err
			}
			d.report.Panes[index].Topics = record.Topics
			if !sameSet(record.Topics, d.subscribe[node]) {
				d.fail(FailSubscription, node, "the subscribed record lists %v, want the topics of owner topics and the test: %v", record.Topics, d.subscribe[node])
			}
		}
		return true, nil
	})
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
func (d *driver) awaitPanes(ctx context.Context, list func(context.Context, string) (map[string]pane, error), start time.Time) (map[string]pane, map[string]time.Duration, error) {
	seen := map[string]time.Duration{}
	var panes map[string]pane
	err := d.wait(ctx, d.opts.Timeouts.PaneStart, "both owner panes", func() (bool, error) {
		var err error
		if panes, err = list(ctx, d.socket); err != nil {
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

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Compact(slices.Sorted(slices.Values(a))), slices.Compact(slices.Sorted(slices.Values(b))))
}

// receivers are the owners whose subscribed record must hold topic.
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

// recordedDeliveries are the deliveries each owner has recorded so far, by event id.
func (d *driver) recordedDeliveries(ctx context.Context) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	for _, node := range d.nodes {
		commits, err := branchCommits(ctx, d.bare, node)
		if err != nil {
			return nil, err
		}
		out[node] = map[string]bool{}
		for _, commit := range commits {
			for _, id := range commit.Trailers[DeliveryTrailer] {
				out[node][id] = true
			}
		}
	}
	return out, nil
}

// arrival publishes one notice on each topic kind the two owners hold, then on the fact and concept topics the
// source owner subscribed to, and waits until every receiver recorded each of them.
func (d *driver) arrival(ctx context.Context) error {
	var held []string
	for _, node := range d.nodes {
		held = append(held, d.topics.Owners[node]...)
	}
	held = slices.Compact(slices.Sorted(slices.Values(held)))
	type notice struct{ kind, topic string }
	var notices []notice
	for _, kind := range TopicKinds {
		for _, topic := range held {
			if topicKind(topic) == kind {
				notices = append(notices, notice{kind, topic})
				break
			}
		}
	}
	for _, topic := range []string{d.opts.Prefix + "fact." + testFact + ".users", d.opts.Prefix + "fact." + testFact + ".needs", d.opts.Prefix + "concepts"} {
		notices = append(notices, notice{strings.TrimPrefix(topic, d.opts.Prefix), topic})
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
	return d.wait(ctx, d.opts.Timeouts.Delivery, "every arrival notice to be recorded", func() (bool, error) {
		recorded, err := d.recordedDeliveries(ctx)
		if err != nil {
			return false, err
		}
		for _, publication := range publications {
			for _, receiver := range publication.Receivers {
				if !recorded[receiver][publication.ID] {
					return false, nil
				}
			}
		}
		return true, nil
	})
}

func (d *driver) need(version string) string {
	return "need:" + ownerpkg.EdgeID(d.source, d.target) + "@" + version
}

// offer is the offer item that answering the need of version queues: the run expects it (expectAnswered) before it
// waits for the answer.
func (d *driver) offer(version string) string { return d.offers[version] }

// expectAnswered records that the need of version must be answered once, with its follow-up offer once. The offer
// is numbered by production's rule, by the revision of the record that answers the need: the target answers the
// run's needs one at a time, each rewriting its answer record as the next revision, so the n-th need it answers
// queues offer:<edge>@r<n>, whatever the need's own version.
func (d *driver) expectAnswered(version string) {
	if d.offers == nil {
		d.offers = map[string]string{}
	}
	offer := fmt.Sprintf("offer:%s@r%d", ownerpkg.EdgeID(d.source, d.target), len(d.offers)+1)
	d.offers[version] = offer
	d.expected.Outcomes = append(d.expected.Outcomes,
		Outcome{Item: d.need(version), Owner: d.target, FollowUp: offer, FollowUpOwner: d.source},
		Outcome{Item: offer, Owner: d.source, Optional: true})
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

// recorded reports whether the target recorded every one of publications.
func (d *driver) recorded(ctx context.Context, publications ...Publication) (bool, error) {
	recorded, err := d.recordedDeliveries(ctx)
	if err != nil {
		return false, err
	}
	for _, publication := range publications {
		if !recorded[d.target][publication.ID] {
			return false, nil
		}
	}
	return true, nil
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

// answered reports whether the need of version is done and its follow-up offer exists.
func (d *driver) answered(ctx context.Context, version string) (bool, error) {
	done, err := d.itemDone(ctx, d.need(version))
	if err != nil || !done {
		return false, err
	}
	return d.itemExists(ctx, d.offer(version))
}

// duplicates enqueues the need r1 twice, and publishes its notice twice under two idempotency keys, as Envoy may
// deliver one message twice: the owner must record both deliveries and answer the need once.
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
	d.expectAnswered("r1")
	return d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered and both of its notices recorded", func() (bool, error) {
		done, err := d.answered(ctx, "r1")
		if err != nil || !done {
			return false, err
		}
		return d.recorded(ctx, copies...)
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
// record, and both notices recorded by the running owner.
func (d *driver) outOfOrder(ctx context.Context) error {
	var notices []Publication
	for _, version := range []string{"r3", "r2"} {
		publication, err := d.enqueueNeed(ctx, "out of order", version, true)
		if err != nil {
			return err
		}
		notices = append(notices, publication)
	}
	d.expectAnswered("r3")
	d.expected.Outcomes = append(d.expected.Outcomes, Outcome{Item: d.need("r2"), Owner: d.target, Superseded: "r3"})
	return d.wait(ctx, d.opts.Timeouts.Item, "r3 to be answered, r2 to be done and both notices recorded", func() (bool, error) {
		done, err := d.answered(ctx, "r3")
		if err != nil || !done {
			return false, err
		}
		if done, err = d.itemDone(ctx, d.need("r2")); err != nil || !done {
			return false, err
		}
		return d.recorded(ctx, notices...)
	})
}

// progress is how far the target has got with item: an answer record (or a completion), a claim, or neither.
func (d *driver) progress(ctx context.Context, item string) (string, bool, error) {
	current, err := d.store.Item(ctx, d.run, item)
	if err != nil {
		return "", false, err
	}
	commits, err := branchCommits(ctx, d.bare, d.target)
	if err != nil {
		return "", false, err
	}
	for _, commit := range commits {
		if slices.Contains(commit.Trailers[itemTrailer], item) {
			return MomentAfterAnswer, current.State == "done", nil
		}
	}
	switch {
	case current.State == "done":
		return MomentAfterAnswer, true, nil
	case current.Holder == d.target:
		return MomentAfterClaim, false, nil
	}
	return MomentBeforeClaim, false, nil
}

// kill enqueues a new need and kills the target's pane at a moment drawn from the owner's progress. The supervisor
// must start the owner again while the item is open, and the item must still be answered once. The need's notice
// is optional: the kill can end the session before it records it, and the ledger item is the wake that lasts.
func (d *driver) kill(index int) func(context.Context) error {
	return func(ctx context.Context) error {
		version := fmt.Sprintf("r%d", 4+index)
		item := d.need(version)
		drawn := moments[d.rng.IntN(len(moments))]
		if _, err := d.enqueueNeed(ctx, "kill", version, false); err != nil {
			return err
		}
		d.expectAnswered(version)
		kill := Kill{Item: item, Drawn: drawn}
		doneAtKill, killedPID := false, 0
		err := d.wait(ctx, d.opts.Timeouts.Item, "the "+drawn+" moment of "+item, func() (bool, error) {
			panes, err := ownerPanes(ctx, d.socket)
			if err != nil {
				return false, err
			}
			progress, done, err := d.progress(ctx, item)
			if err != nil {
				return false, err
			}
			p, live := panes[d.target]
			if !live {
				if done {
					return false, fmt.Errorf("%s was answered while %s had no pane to kill", item, d.target)
				}
				return false, nil
			}
			if momentRank(progress) < momentRank(drawn) {
				return false, nil
			}
			if err := killPane(ctx, d.socket, p.ID); err != nil {
				return false, err
			}
			kill.Observed, kill.Pane, kill.At, doneAtKill, killedPID = progress, p.ID, time.Now().UTC(), done, p.PID
			return true, nil
		})
		if err != nil {
			return err
		}
		d.logf("killed %s of %s at %s (drawn %s)", kill.Pane, d.target, kill.Observed, drawn)
		err = d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered after the kill", func() (bool, error) {
			if kill.RelaunchSeconds == 0 && !doneAtKill {
				panes, err := ownerPanes(ctx, d.socket)
				if err != nil {
					return false, err
				}
				// A pane id can come back when the killed pane was the server's last, so a relaunch is a new process.
				if p, live := panes[d.target]; live && p.PID != killedPID {
					kill.RelaunchSeconds = time.Since(kill.At).Seconds()
				}
			}
			return d.answered(ctx, version)
		})
		d.report.KillLog = append(d.report.KillLog, kill)
		return err
	}
}

// restart enqueues a new need, waits until the target's pane runs, restarts the daemon, and waits for the need to be
// answered once. The need's notice is required when the target's pane was running when it was published: a running
// pane has answered an item since it started, so its session is subscribed. After the restart a second notice must
// reach the pane that the restart left running.
func (d *driver) restart(ctx context.Context) error {
	version := fmt.Sprintf("r%d", 4+d.opts.Kills)
	item := d.need(version)
	panes, err := ownerPanes(ctx, d.socket)
	if err != nil {
		return err
	}
	_, running := panes[d.target]
	wake, err := d.enqueueNeed(ctx, "restart", version, running)
	if err != nil {
		return err
	}
	d.expectAnswered(version)
	restart := &Restart{Item: item}
	d.report.Restart = restart
	if err := d.wait(ctx, d.opts.Timeouts.PaneStart, "the pane of "+d.target, func() (bool, error) {
		panes, err := ownerPanes(ctx, d.socket)
		if err != nil {
			return false, err
		}
		restart.PanesBefore = paneIDs(panes)
		_, live := panes[d.target]
		return live, nil
	}); err != nil {
		return err
	}
	start := time.Now()
	if err := d.stopDaemon(); err != nil {
		return err
	}
	restart.StopSeconds = time.Since(start).Seconds()
	started := time.Now()
	if err := d.startDaemon(ctx); err != nil {
		return err
	}
	restart.StartSeconds = time.Since(started).Seconds()
	err = d.wait(ctx, d.opts.Timeouts.Item, item+" to be answered after the restart", func() (bool, error) {
		return d.answered(ctx, version)
	})
	restart.ItemSeconds = time.Since(start).Seconds()
	if panes, listErr := ownerPanes(ctx, d.socket); listErr == nil {
		restart.PanesAfter = paneIDs(panes)
	}
	if err != nil {
		return err
	}
	after, err := d.notifyTarget(ctx, "restart", item, true)
	if err != nil {
		return err
	}
	notices := []Publication{after}
	if running {
		notices = append(notices, wake)
	}
	return d.wait(ctx, d.opts.Timeouts.Delivery, "the restart's notices to be recorded", func() (bool, error) {
		return d.recorded(ctx, notices...)
	})
}

func paneIDs(panes map[string]pane) []string {
	var ids []string
	for owner, p := range panes {
		ids = append(ids, owner+"="+p.ID)
	}
	slices.Sort(ids)
	return ids
}

// collect reads the run's ledger events and the owners' branches, and each delivery's latency.
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
	for _, publication := range d.report.Published {
		for _, receiver := range publication.Receivers {
			var saw Saw
			found, err := readRecord(ctx, d.bare, receiver, SawRecord(d.root, receiver, publication.ID), &saw)
			if err != nil {
				return recorded, err
			}
			if found {
				d.report.Deliveries = append(d.report.Deliveries, Latency{Owner: receiver, ID: publication.ID, Topic: saw.Topic, Seconds: saw.ReceivedAt.Sub(publication.At).Seconds()})
			}
		}
	}
	return recorded, nil
}

// teardown stops the daemon and every pane on its socket, and removes the bearer files the run wrote.
func (d *driver) teardown(ctx context.Context) error {
	var errs []error
	if err := d.stopDaemon(); err != nil {
		errs = append(errs, err)
	}
	if d.socket != "" {
		if err := killServer(ctx, d.socket); err != nil {
			errs = append(errs, err)
		}
	}
	if d.tokenDir != "" {
		if err := os.RemoveAll(d.tokenDir); err != nil {
			errs = append(errs, err)
		}
	}
	if d.store != nil {
		d.store.Close()
	}
	return errors.Join(errs...)
}
