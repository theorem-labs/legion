package messagetest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// LaunchVariables are the owner launch environment of shared contract C8, as INFRA-137 names it: the node id, the
// ledger repository and the ledger DSN file. (A name ending _KEY has a credential's shape, which the daemon strips
// from every pane, so the node id travels as LEGION_OWNER.)
var LaunchVariables = []string{"LEGION_OWNER", "LEGION_LEDGER_REPOSITORY", "LEGION_LEDGER_DSN_FILE"}

// pane is one tmux pane of the daemon's socket that runs an owner: the process that holds the launch variables,
// its environment's launch variables and its working directory.
type pane struct {
	ID    string
	PID   int
	Owner string
	Env   map[string]string
	Dir   string
}

// ownerPanes lists the panes on the tmux socket whose process tree holds LEGION_OWNER, by owner. A socket with no
// server has none.
func ownerPanes(ctx context.Context, socket string) (map[string]pane, error) {
	out, err := exec.CommandContext(ctx, "tmux", "-L", socket, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}").CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "no server running") || strings.Contains(text, "error connecting") || strings.Contains(text, "No such file") {
			return map[string]pane{}, nil
		}
		return nil, fmt.Errorf("tmux -L %s list-panes: %w: %s", socket, err, bytes.TrimSpace(out))
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
			found.ID = id
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

func killPane(ctx context.Context, socket, id string) error {
	if out, err := exec.CommandContext(ctx, "tmux", "-L", socket, "kill-pane", "-t", id).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux -L %s kill-pane -t %s: %w: %s", socket, id, err, bytes.TrimSpace(out))
	}
	return nil
}

// killServer stops every pane on the socket; a socket with no server is already stopped.
func killServer(ctx context.Context, socket string) error {
	out, err := exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no server running") && !strings.Contains(string(out), "error connecting") {
		return fmt.Errorf("tmux -L %s kill-server: %w: %s", socket, err, bytes.TrimSpace(out))
	}
	return nil
}

// paneCommands lists every pane of the socket with its start command, for a report of panes without an owner.
func paneCommands(ctx context.Context, socket string) string {
	out, err := exec.CommandContext(ctx, "tmux", "-L", socket, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_start_command}").CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	if text := strings.TrimSpace(string(out)); text != "" {
		return text
	}
	return "none"
}
