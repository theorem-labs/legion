package messagetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LaunchVariables are the owner launch environment of shared contract C8, as the agreed supervisor contract names
// it: the node id, the ledger repository and the ledger DSN file. (A name ending _KEY has a credential's shape, which
// the daemon strips from every pane, so the node id travels as LEGION_OWNER.)
var LaunchVariables = []string{"LEGION_OWNER", "LEGION_LEDGER_REPOSITORY", "LEGION_LEDGER_DSN_FILE"}

// pane is one tmux pane of the daemon's socket that runs an owner: the pane's own process (Root), the process that
// holds the launch variables, its environment's launch variables and its working directory.
type pane struct {
	ID    string
	Root  int
	PID   int
	Owner string
	Env   map[string]string
	Dir   string
}

// tmuxServer is the daemon's private tmux server as tmux finds it: the socket name the daemon gives it (-L), in the
// directory TMUX_TMPDIR names. The driver gives the daemon the run's own TMUX_TMPDIR (the daemon keeps that variable
// for each tmux command it runs), and runs each tmux command of its own under the same one, so both reach one server.
// An empty tmpDir is tmux's default directory.
type tmuxServer struct{ socket, tmpDir string }

// ErrTmuxSocketTooLong is an output directory too long to hold a run's tmux socket.
var ErrTmuxSocketTooLong = errors.New("the run's tmux socket would not fit a Unix socket address")

// maxSocketPath is the longest path a Unix socket address holds: sun_path is 108 bytes with its NUL, and tmux refuses
// a longer path with ENAMETOOLONG (tmux 3.7b client.c:114-117, server.c:116-119).
const maxSocketPath = 107

// runTmux is the private tmux server of a run in out: the socket of the daemon of project, in <out>/tmux, which the
// driver gives the daemon and its own tmux commands as TMUX_TMPDIR.
func runTmux(out, project string) tmuxServer {
	return tmuxServer{socket: DaemonSocket(project), tmpDir: filepath.Join(out, "tmux")}
}

// fits refuses a socket path that would not fit a Unix socket address, with ErrTmuxSocketTooLong.
func (s tmuxServer) fits() error {
	if path := s.socketPath(); len(path) > maxSocketPath {
		return fmt.Errorf("%w: %s is %d bytes, and a Unix socket path holds at most %d; choose a shorter output directory",
			ErrTmuxSocketTooLong, path, len(path), maxSocketPath)
	}
	return nil
}

// CheckOut refuses an output directory too long to hold the tmux socket of a run in it, with ErrTmuxSocketTooLong.
// Every run's socket path has one length, whatever the random digits of its name, so the operator's command checks
// its output directory before it starts a run; the run's setup checks its own again.
func CheckOut(out string) error {
	run, err := runName(time.Now())
	if err != nil {
		return err
	}
	return runTmux(out, daemonProject(run)).fits()
}

// socketPath is where tmux puts the server's socket: realpath(TMUX_TMPDIR)/tmux-<uid>/<socket> (tmux 3.7b tmux.c,
// make_label). The links of the part of tmpDir that exists are resolved, and the rest, which the driver makes, is
// taken as it is.
func (s tmuxServer) socketPath() string {
	base := filepath.Join("tmux-"+strconv.Itoa(os.Getuid()), s.socket)
	for dir, rest := s.tmpDir, ""; ; dir, rest = filepath.Dir(dir), filepath.Join(filepath.Base(dir), rest) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest, base)
		}
		if filepath.Dir(dir) == dir {
			return filepath.Join(s.tmpDir, base)
		}
	}
}

// command is `tmux -L <socket> args...` under the server's TMUX_TMPDIR.
func (s tmuxServer) command(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "tmux", append([]string{"-L", s.socket}, args...)...)
	if s.tmpDir != "" {
		command.Env = append(os.Environ(), "TMUX_TMPDIR="+s.tmpDir)
	}
	return command
}

// tmuxExiting is what tmux says when the server closes the client's connection unanswered. An exiting server does so
// to each new connection until its last client has left (server_accept in tmux 3.4's server.c), and kill-server
// returns before that: such a server has no pane, and it is stopped.
const tmuxExiting = "server exited unexpectedly"

// tmuxExitWait bounds how long a command asks an exiting server again, every tmuxExitPoll, until its socket refuses.
const (
	tmuxExitWait = time.Second
	tmuxExitPoll = 20 * time.Millisecond
)

// combinedOutput runs `tmux -L <socket> args...` and returns its output. The answer of an exiting server (tmuxExiting)
// is asked again until the socket refuses or a server answers, for up to tmuxExitWait; exiting reports that the last
// answer was still that one. Any other error is returned as tmux gave it.
func (s tmuxServer) combinedOutput(ctx context.Context, args ...string) (out []byte, exiting bool, err error) {
	deadline := time.Now().Add(tmuxExitWait)
	for {
		out, err = s.command(ctx, args...).CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte(tmuxExiting)) {
			return out, false, err
		}
		if !time.Now().Before(deadline) {
			return out, true, err
		}
		select {
		case <-ctx.Done():
			return out, true, err
		case <-time.After(tmuxExitPoll):
		}
	}
}

// ownerPanes lists the panes of the server whose process tree holds LEGION_OWNER, by owner. A socket with no server
// has none, and so does an exiting server (combinedOutput), and a server with no session yet, which tmux answers with
// "no current target": the supervisor's private server is up before its first owner's session exists.
func (s tmuxServer) ownerPanes(ctx context.Context) (map[string]pane, error) {
	out, exiting, err := s.combinedOutput(ctx, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}")
	if err != nil {
		text := string(out)
		if exiting || strings.Contains(text, "no server running") || strings.Contains(text, "error connecting") ||
			strings.Contains(text, "No such file") || strings.Contains(text, "no current target") {
			return map[string]pane{}, nil
		}
		return nil, fmt.Errorf("tmux -L %s list-panes: %w: %s", s.socket, err, bytes.TrimSpace(out))
	}
	panes := map[string]pane{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, pidText, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			continue
		}
		if found, ok := ownerProcess(pid); ok {
			found.ID, found.Root = id, pid
			panes[found.Owner] = found
		}
	}
	return panes, nil
}

// ownerProcess finds, in the process tree under pid, the first process whose environment holds LEGION_OWNER.
func ownerProcess(root int) (pane, bool) {
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		env := processEnv(pid)
		if owner := env["LEGION_OWNER"]; owner != "" {
			launch := map[string]string{}
			for _, name := range LaunchVariables {
				if value, ok := env[name]; ok {
					launch[name] = value
				}
			}
			dir, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
			return pane{PID: pid, Owner: owner, Env: launch, Dir: dir}, true
		}
		queue = append(queue, children(pid)...)
	}
	return pane{}, false
}

func processEnv(pid int) map[string]string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, entry := range bytes.Split(data, []byte{0}) {
		if name, value, ok := strings.Cut(string(entry), "="); ok {
			env[name] = value
		}
	}
	return env
}

func children(pid int) []int {
	tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	if err != nil {
		return nil
	}
	var out []int
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "children"))
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(field); err == nil {
				out = append(out, child)
			}
		}
	}
	return out
}

func (s tmuxServer) killPane(ctx context.Context, id string) error {
	if out, err := s.command(ctx, "kill-pane", "-t", id).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux -L %s kill-pane -t %s: %w: %s", s.socket, id, err, bytes.TrimSpace(out))
	}
	return nil
}

// killServer stops every pane of the server; a socket with no server, or an exiting server (combinedOutput), is already
// stopped.
func (s tmuxServer) killServer(ctx context.Context) error {
	out, exiting, err := s.combinedOutput(ctx, "kill-server")
	if err != nil && !exiting && !strings.Contains(string(out), "no server running") && !strings.Contains(string(out), "error connecting") {
		return fmt.Errorf("tmux -L %s kill-server: %w: %s", s.socket, err, bytes.TrimSpace(out))
	}
	return nil
}

// paneCommands lists every pane of the server with its start command, for a report of panes without an owner.
func (s tmuxServer) paneCommands(ctx context.Context) string {
	out, err := s.command(ctx, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_start_command}").CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	if text := strings.TrimSpace(string(out)); text != "" {
		return text
	}
	return "none"
}
