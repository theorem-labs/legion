package messagetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// Revisions are what a run ran: the owner command's build and Go modules, the supervisor's and omp's own version
// output, tmux and git, and the SHA-256 of each graph input (plan Task 1.8 step 6).
type Revisions struct {
	Owner        Build             `json:"owner"`
	Dependencies []Module          `json:"dependencies"`
	Legion       string            `json:"legion"`
	Omp          string            `json:"omp"`
	Tmux         string            `json:"tmux"`
	Git          string            `json:"git"`
	Graph        map[string]string `json:"graph"`
}

// Build is the owner command's main module as the Go toolchain stamped it.
type Build struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Time      string `json:"time"`
	Modified  string `json:"modified"`
	GoVersion string `json:"go_version"`
}

// Module is one Go module the owner command is built with.
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum,omitempty"`
}

const versionTimeout = 10 * time.Second

// collectRevisions reads the revisions of a run: the owner's build from this executable, `legion version`,
// `omp --version` (omp "" when the supervisor's PATH has none), `tmux -V`, `git --version`, and the digest of each
// graph input in graphDir. A tool that cannot answer is recorded as unavailable, with why.
func collectRevisions(ctx context.Context, legion, omp, graphDir string) Revisions {
	revisions := Revisions{
		Legion: versionOutput(ctx, legion, "version"),
		Tmux:   versionOutput(ctx, "tmux", "-V"),
		Git:    versionOutput(ctx, "git", "--version"),
		Graph:  map[string]string{},
	}
	if omp == "" {
		revisions.Omp = "unavailable: omp is not on the supervisor's PATH"
	} else {
		revisions.Omp = versionOutput(ctx, omp, "--version")
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		revisions.Owner = Build{Path: info.Main.Path, Version: info.Main.Version, GoVersion: info.GoVersion}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revisions.Owner.Revision = setting.Value
			case "vcs.time":
				revisions.Owner.Time = setting.Value
			case "vcs.modified":
				revisions.Owner.Modified = setting.Value
			}
		}
		for _, dep := range info.Deps {
			module := dep
			if dep.Replace != nil {
				module = dep.Replace
			}
			revisions.Dependencies = append(revisions.Dependencies, Module{Path: dep.Path, Version: module.Version, Sum: module.Sum})
		}
	}
	for _, name := range []string{"graph.json", "owners.json", "topics.json"} {
		data, err := os.ReadFile(filepath.Join(graphDir, name))
		if err != nil {
			revisions.Graph[name] = "unavailable: " + err.Error()
			continue
		}
		revisions.Graph[name] = ownerpkg.ContentHash(data)
	}
	return revisions
}

// versionOutput is the trimmed output of `command arg`, or why it gave none.
func versionOutput(ctx context.Context, command, arg string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, command, arg).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return fmt.Sprintf("unavailable: %s %s: %v", command, arg, err)
		}
		return fmt.Sprintf("unavailable: %s %s: %v: %s", command, arg, err, text)
	}
	return text
}

// daemonTokenFile is the file the supervisor reads the Envoy bearer from: ENVOY_TOKEN_FILE when the operator named
// one, else a file the run writes in the private directory dir (mode 0700, the file 0600), or none when there is no
// token. written reports whether the run wrote it; the teardown removes dir then.
func daemonTokenFile(getenv func(string) string, token, dir string) (path string, written bool, err error) {
	if named := strings.TrimSpace(getenv("ENVOY_TOKEN_FILE")); named != "" {
		return named, false, nil
	}
	if token == "" {
		return "", false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, "envoy-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// LookPath finds an executable name on the given PATH, which may differ from the process's own.
func LookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: %s on PATH %q", exec.ErrNotFound, name, path)
}
