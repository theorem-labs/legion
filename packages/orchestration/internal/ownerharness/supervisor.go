package ownerharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
	"github.com/sjawhar/legion/orchestration/owner/messagetest"
	"gopkg.in/yaml.v3"
)

// Config is the daemon's minimal standing-owners configuration (shared contract 10, C33, with the standing_owners
// keys of C7 and the operator bearer the daemon requires at boot): what the harness supervisor reads from `legion
// start --config FILE`. OMP is the omp it runs in every pane, from LEGION_OMP_PATH, as the daemon resolves it.
type Config struct {
	Project           string         `yaml:"project"`
	StateDir          string         `yaml:"state_dir"`
	PostgresDSN       string         `yaml:"postgres_dsn"`
	Runtime           string         `yaml:"runtime"`
	Port              int            `yaml:"port"`
	OperatorTokenFile string         `yaml:"operator_token_file"`
	OmpInvocation     string         `yaml:"omp_invocation"`
	EnvoyURL          string         `yaml:"envoy_url"`
	NATSURLs          []string       `yaml:"nats_urls"`
	EnvoyTokenFile    string         `yaml:"envoy_token_file"`
	StandingOwners    StandingOwners `yaml:"standing_owners"`
	OMP               string         `yaml:"-"`
}

// StandingOwners is the standing_owners block: the ledger, the repository and limits, the named owners, the
// templates of graph owners by kind, and the owners never started.
type StandingOwners struct {
	LedgerDSNFile        string              `yaml:"ledger_dsn_file"`
	Repository           string              `yaml:"repository"`
	MaxRunning           int                 `yaml:"max_running"`
	BudgetUSD            float64             `yaml:"budget_usd"`
	LedgerTimeoutSeconds int                 `yaml:"ledger_timeout_seconds"`
	ReconcileSeconds     int                 `yaml:"reconcile_seconds"`
	Owners               []NamedOwner        `yaml:"owners"`
	Templates            map[string]Template `yaml:"templates"`
	NeverStart           []string            `yaml:"never_start"`
}

// NamedOwner is a role owner (the librarian, a fact owner) started under its own name.
type NamedOwner struct {
	Name     string `yaml:"name"`
	Template `yaml:",inline"`
}

// Template starts an owner: its prompt, its harness and its cost cap.
type Template struct {
	PromptFile string  `yaml:"prompt_file"`
	Harness    string  `yaml:"harness"`
	MaxCostUSD float64 `yaml:"max_cost_usd"`
}

var (
	graphID  = regexp.MustCompile(`^(function|struct|global|percpu|subsystem):\S+$`)
	roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	kinds    = []string{"function", "struct", "global", "percpu", "subsystem"}
)

// LoadConfig reads and checks the configuration as the daemon would: no key outside the minimal file, the daemon's
// database in the file or LEGION_POSTGRES_DSN, an operator bearer file that holds a token, the omp that
// LEGION_OMP_PATH names (the daemon never looks omp up on PATH), a ledger DSN file only its owner can read, and owners
// or templates whose prompts exist and whose harness is omp. Relative paths are relative to the file.
func LoadConfig(path string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	resolve := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	resolve(&config.StateDir)
	resolve(&config.EnvoyTokenFile)
	resolve(&config.OperatorTokenFile)
	owners := &config.StandingOwners
	resolve(&owners.LedgerDSNFile)
	var all []Template
	for index := range owners.Owners {
		resolve(&owners.Owners[index].PromptFile)
		all = append(all, owners.Owners[index].Template)
	}
	for kind, template := range owners.Templates {
		resolve(&template.PromptFile)
		owners.Templates[kind] = template
		all = append(all, template)
	}
	if config.PostgresDSN == "" {
		config.PostgresDSN = strings.TrimSpace(getenv("LEGION_POSTGRES_DSN"))
	}
	switch {
	case messagetest.ProjectToken(config.Project) == "":
		return Config{}, errors.New("project is required and must hold a letter or digit")
	case config.StateDir == "":
		return Config{}, errors.New("state_dir is required")
	case config.PostgresDSN == "":
		return Config{}, errors.New("postgres_dsn is required, in the file or as LEGION_POSTGRES_DSN")
	case config.Runtime != "" && config.Runtime != "tmux":
		return Config{}, fmt.Errorf("runtime %q: standing owners run under tmux", config.Runtime)
	case config.Port < 0 || config.Port > 65535:
		return Config{}, fmt.Errorf("port %d is not a TCP port", config.Port)
	case owners.LedgerDSNFile == "" || owners.Repository == "":
		return Config{}, errors.New("standing_owners needs ledger_dsn_file and repository")
	case owners.MaxRunning <= 0 || owners.BudgetUSD <= 0 || math.IsInf(owners.BudgetUSD, 0):
		return Config{}, errors.New("standing_owners needs a positive max_running and budget_usd")
	case len(owners.Owners) == 0 && len(owners.Templates) == 0:
		return Config{}, errors.New("standing_owners needs owners or templates")
	}
	// The daemon's own boot refusals: its internal/daemon/bootreads.go and internal/omplaunch/omplaunch.go.
	if config.OperatorTokenFile == "" {
		return Config{}, errors.New("operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names")
	}
	if token, err := os.ReadFile(config.OperatorTokenFile); err != nil {
		return Config{}, fmt.Errorf("operator_token_file names %s, which could not be read: %w", config.OperatorTokenFile, err)
	} else if strings.TrimSpace(string(token)) == "" {
		return Config{}, fmt.Errorf("operator_token_file names %s, which is empty", config.OperatorTokenFile)
	}
	if config.OMP, err = ompPath(config.OmpInvocation, getenv); err != nil {
		return Config{}, err
	}
	info, err := os.Stat(owners.LedgerDSNFile)
	if err != nil {
		return Config{}, fmt.Errorf("standing_owners.ledger_dsn_file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("standing_owners.ledger_dsn_file %s must be a regular file only its owner can read (mode 0600)", owners.LedgerDSNFile)
	}
	for kind := range owners.Templates {
		if !slices.Contains(kinds, kind) {
			return Config{}, fmt.Errorf("standing_owners.templates.%s: the kinds are %v", kind, kinds)
		}
	}
	for _, owner := range owners.Owners {
		if !roleName.MatchString(owner.Name) {
			return Config{}, fmt.Errorf("standing_owners.owners: %q is not a role name", owner.Name)
		}
	}
	for _, template := range all {
		if template.Harness != "omp" {
			return Config{}, fmt.Errorf("harness %q: standing owners run omp", template.Harness)
		}
		if template.MaxCostUSD <= 0 {
			return Config{}, errors.New("max_cost_usd must be positive")
		}
		if _, err := os.Stat(template.PromptFile); err != nil {
			return Config{}, fmt.Errorf("prompt_file: %w", err)
		}
	}
	return config, nil
}

// ompPath is the omp every pane runs: LEGION_OMP_PATH, an absolute path to an executable, as the daemon takes it. The
// daemon's other form, omp_invocation `mise x <tool> -- omp`, needs mise, which this stand-in does not run. As the
// daemon does, it refuses a configuration that names neither, and never looks omp up on PATH. It runs the path as
// named, not as its real path: the test-only stand-ins are links that act by the name they are run as.
func ompPath(invocation string, getenv func(string) string) (string, error) {
	configured := strings.TrimSpace(getenv("LEGION_OMP_PATH"))
	switch {
	case configured == "" && invocation == "":
		return "", errors.New("omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path")
	case configured == "":
		return "", fmt.Errorf("omp_invocation %q: the test-only stand-in runs omp only from LEGION_OMP_PATH", invocation)
	case !filepath.IsAbs(configured):
		return "", errors.New("LEGION_OMP_PATH must be an absolute executable path")
	}
	info, err := os.Stat(configured)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("LEGION_OMP_PATH is not an executable: %s", configured)
	}
	return configured, nil
}

type supervisor struct {
	config    Config
	store     *ledger.Store
	dsnFile   string
	socket    string
	token     string
	panes     map[string]string // owner → tmux pane id
	pending   map[string]bool   // owners with work that wait for capacity
	refused   map[string]int64  // owner → the work revision of its last refusal
	spent     float64
	log       io.Writer
	probeTick time.Duration
}

// Supervise runs the standing-owner supervisor until ctx ends: it starts an owner in tmux when the ledger names it
// with open work, gives the owner the launch variables of C8 and a working directory named by its encoded id (C9),
// frees the leases of an owner whose pane died and starts it again while work remains, and adopts the panes it finds
// at boot. When ctx ends it stops supervising and leaves every pane running, as the daemon does on SIGTERM.
func Supervise(ctx context.Context, config Config, log io.Writer) error {
	dsn, err := os.ReadFile(config.StandingOwners.LedgerDSNFile)
	if err != nil {
		return err
	}
	store, err := ledger.Open(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return fmt.Errorf("open the ledger: %w", err)
	}
	defer store.Close()
	listener, err := ledger.Listen(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return fmt.Errorf("listen on the ledger: %w", err)
	}
	// A pgx connection serves one goroutine at a time: the waiter stops before the listener closes.
	wakes, waiting := make(chan struct{}, 1), make(chan struct{})
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer func() {
		stopWaiting()
		<-waiting
		_ = listener.Close(context.Background())
	}()
	go func() {
		defer close(waiting)
		for {
			if _, err := listener.Wait(waitCtx); err != nil {
				return
			}
			select {
			case wakes <- struct{}{}:
			default:
			}
		}
	}()
	s := &supervisor{
		config: config, store: store, dsnFile: config.StandingOwners.LedgerDSNFile, socket: messagetest.DaemonSocket(config.Project),
		token: messagetest.ProjectToken(config.Project), panes: map[string]string{}, pending: map[string]bool{}, refused: map[string]int64{},
		log: log, probeTick: 200 * time.Millisecond,
	}
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return err
	}
	server, err := s.serve(ctx)
	if err != nil {
		return err
	}
	defer server.Close()
	if err := s.adopt(ctx); err != nil {
		return err
	}
	repo := config.StandingOwners.Repository
	cursor, err := store.LatestEventID(ctx, repo)
	if err != nil {
		return err
	}
	reconcileEvery := time.Duration(config.StandingOwners.ReconcileSeconds) * time.Second
	if reconcileEvery <= 0 {
		reconcileEvery = 5 * time.Minute
	}
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	probe := time.NewTicker(s.probeTick)
	defer probe.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			s.logf("stopping; the owner panes keep running")
			return nil
		case <-wakes:
			owners, last, err := store.OwnerWakes(ctx, repo, cursor)
			if err != nil {
				s.logf("read owner wakes: %v", err)
				continue
			}
			cursor = last
			for _, owner := range owners {
				s.admit(ctx, owner)
			}
		case <-probe.C:
			s.probe(ctx)
		case <-reconcile.C:
			s.sweep(ctx)
		}
	}
}

func (s *supervisor) logf(format string, args ...any) {
	fmt.Fprintf(s.log, "%s supervisor %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

// serve answers on bind 127.0.0.1 and the configured port, as the daemon's API does; the driver waits for it.
func (s *supervisor) serve(ctx context.Context) (*http.Server, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.config.Port)))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"project": s.config.Project})
	})}
	go func() { _ = server.Serve(listener) }()
	s.logf("listening on %s", listener.Addr())
	return server, nil
}

func (s *supervisor) tmux(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", s.socket}, args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux -L %s %s: %w: %s", s.socket, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

// livePanes lists the socket's panes and the owner each runs; no server means none.
func (s *supervisor) livePanes(ctx context.Context) (map[string]string, error) {
	// tmux prints a control character of a format as "_", so the fields are separated by a space: no owner id holds one.
	out, err := s.tmux(ctx, "list-panes", "-a", "-F", "#{pane_id} #{@legion_owner}")
	if err != nil {
		if strings.Contains(out, "no server running") || strings.Contains(out, "error connecting") {
			return map[string]string{}, nil
		}
		return nil, err
	}
	panes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if id, owner, ok := strings.Cut(line, " "); ok && id != "" {
			panes[id] = owner
		}
	}
	return panes, nil
}

func (s *supervisor) adopt(ctx context.Context) error {
	panes, err := s.livePanes(ctx)
	if err != nil {
		return err
	}
	for id, owner := range panes {
		if owner != "" {
			s.panes[owner] = id
			s.logf("adopted the pane %s of %s", id, owner)
		}
	}
	return nil
}

func (s *supervisor) sweep(ctx context.Context) {
	owners, err := s.store.OpenOwners(ctx, s.config.StandingOwners.Repository)
	if err != nil {
		s.logf("reconcile: %v", err)
		return
	}
	for _, owner := range owners {
		s.admit(ctx, owner)
	}
}

// probe finds the owners whose pane is gone, frees their leases, and starts each again while it has work; then it
// admits the owners that waited for capacity.
func (s *supervisor) probe(ctx context.Context) {
	panes, err := s.livePanes(ctx)
	if err != nil {
		s.logf("probe: %v", err)
		return
	}
	for owner, id := range s.panes {
		if _, live := panes[id]; live {
			continue
		}
		delete(s.panes, owner)
		freed, err := s.store.ReleaseHolder(ctx, s.config.StandingOwners.Repository, owner, "owner pane stopped")
		if err != nil {
			s.logf("release the leases of %s: %v", owner, err)
		}
		s.logf("the pane %s of %s stopped; freed %d leases", id, owner, len(freed))
		s.pending[owner] = true
	}
	for _, owner := range slices.Sorted(maps.Keys(s.pending)) {
		s.admit(ctx, owner)
	}
}

func (s *supervisor) template(owner string) (Template, string) {
	if graphID.MatchString(owner) {
		kind, _, _ := strings.Cut(owner, ":")
		if template, ok := s.config.StandingOwners.Templates[kind]; ok {
			return template, ""
		}
		return Template{}, "no template for the kind " + kind
	}
	if roleName.MatchString(owner) {
		for _, named := range s.config.StandingOwners.Owners {
			if named.Name == owner {
				return named.Template, ""
			}
		}
		return Template{}, "a role with no owners entry"
	}
	return Template{}, "an id outside the graph-id and role forms"
}

func (s *supervisor) admit(ctx context.Context, owner string) {
	repo := s.config.StandingOwners.Repository
	if _, running := s.panes[owner]; running || slices.Contains(s.config.StandingOwners.NeverStart, owner) {
		delete(s.pending, owner)
		return
	}
	work, err := s.store.HasOpenWork(ctx, repo, owner)
	if err != nil || !work {
		delete(s.pending, owner)
		return
	}
	template, refusal := s.template(owner)
	if refusal != "" {
		delete(s.pending, owner)
		s.refuse(ctx, owner, refusal)
		return
	}
	if len(s.panes) >= s.config.StandingOwners.MaxRunning {
		s.pending[owner] = true
		return
	}
	if s.spent+template.MaxCostUSD > s.config.StandingOwners.BudgetUSD {
		delete(s.pending, owner)
		s.refuse(ctx, owner, "budget_usd is spent")
		return
	}
	if err := s.launch(ctx, owner, template); err != nil {
		s.logf("start %s: %v", owner, err)
		s.pending[owner] = true
		return
	}
	delete(s.pending, owner)
	s.spent += template.MaxCostUSD
}

// refuse records a refusal once per work revision of the owner, in <state_dir>/refusals.jsonl. (The daemon's C9
// refusal is an owner.refused event in the ledger's owner_refusals; this stand-in does not model the ledger side.)
func (s *supervisor) refuse(ctx context.Context, owner, reason string) {
	revision, err := s.store.OwnerWorkRevision(ctx, s.config.StandingOwners.Repository, owner)
	if err != nil {
		s.logf("the work revision of %s: %v", owner, err)
		return
	}
	if last, ok := s.refused[owner]; ok && last == revision {
		return
	}
	s.refused[owner] = revision
	line, _ := json.Marshal(map[string]any{"owner": owner, "revision": revision, "reason": reason, "at": time.Now().UTC()})
	file, err := os.OpenFile(filepath.Join(s.config.StateDir, "refusals.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.logf("record the refusal of %s: %v", owner, err)
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
	s.logf("refused %s: %s", owner, reason)
}

func shellQuote(words ...string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

// launch starts owner in a new tmux window: its working directory is <state_dir>/owners/<claim token>, its
// environment holds the launch variables, and it runs the configured omp with the template's prompt.
func (s *supervisor) launch(ctx context.Context, owner string, template Template) error {
	token := "legion-" + s.token + "-owner-" + messagetest.EncodeOwnerName(owner)
	workdir := filepath.Join(s.config.StateDir, "owners", token)
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return err
	}
	startFile := filepath.Join(s.config.StateDir, "claims", token, "start.md")
	if err := os.MkdirAll(filepath.Dir(startFile), 0o700); err != nil {
		return err
	}
	_ = os.Remove(startFile)
	start := fmt.Sprintf("You are the owner %s of ledger repository %s. Work through your queue as your instructions say.\n", owner, s.config.StandingOwners.Repository)
	if err := os.WriteFile(startFile, []byte(start), 0o400); err != nil {
		return err
	}
	env := []string{
		"LEGION_OWNER=" + owner, "LEGION_LEDGER_REPOSITORY=" + s.config.StandingOwners.Repository, "LEGION_LEDGER_DSN_FILE=" + s.dsnFile,
		"LEGION_OWNER_START_FILE=" + startFile, "LEGION_ROLE=owner", "LEGION_STATE_DIR=" + s.config.StateDir, "ENVOY_URL=" + s.config.EnvoyURL,
	}
	if len(s.config.NATSURLs) > 0 {
		env = append(env, "ENVOY_NATS_URL="+strings.Join(s.config.NATSURLs, ","))
	}
	if s.config.EnvoyTokenFile != "" {
		env = append(env, "ENVOY_TOKEN_FILE="+s.config.EnvoyTokenFile)
	}
	args := []string{"new-session", "-d", "-s", s.socket}
	if _, err := s.tmux(ctx, "has-session", "-t", s.socket); err == nil {
		args = []string{"new-window", "-d", "-t", s.socket + ":"}
	}
	args = append(args, "-c", workdir)
	for _, pair := range env {
		args = append(args, "-e", pair)
	}
	args = append(args, "-P", "-F", "#{pane_id} #{pane_pid}", shellQuote("exec", s.config.OMP, "--system-prompt-file", template.PromptFile))
	out, err := s.tmux(ctx, args...)
	if err != nil {
		return err
	}
	id, pidText, _ := strings.Cut(strings.TrimSpace(out), " ")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return fmt.Errorf("tmux reported the pane %q with no pane pid: %w", strings.TrimSpace(out), err)
	}
	if _, err := s.tmux(ctx, "set-option", "-p", "-t", id, "@legion_owner", owner); err != nil {
		return err
	}
	s.panes[owner] = id
	s.logf("started %s in the pane %s (%s)", owner, id, workdir)
	// The daemon's launch line (supervise: launched, packages/daemon/internal/supervise/machine.go), with the pane's
	// incarnation as the daemon records it: the driver's restart check reads it.
	incarnation, _, err := messagetest.ProcessIncarnation(pid)
	if err != nil {
		return err
	}
	line, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "level": "INFO", "msg": messagetest.LaunchedLine, "owner": owner, "incarnation": incarnation})
	_, _ = fmt.Fprintf(s.log, "%s\n", line)
	return nil
}
