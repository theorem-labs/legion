package messagetest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// PaneProcess is one owner pane: its tmux id, its root process and that process's incarnation.
type PaneProcess struct {
	Pane        string `json:"pane"`
	PID         int    `json:"pid"`
	Incarnation string `json:"incarnation"`
}

// ProcessIncarnation is pid's identity as the daemon records a pane's: `<pid>:<start ticks>`, the start ticks being
// field 22 of /proc/<pid>/stat, counted after the command name's closing parenthesis (the daemon's
// runtime/tmux.incarnation and parseProcStatStartTicks). alive is false when the process is gone.
func ProcessIncarnation(pid int) (incarnation string, alive bool, err error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", false, fmt.Errorf("/proc/%d/stat has no command name", pid)
	}
	fields := strings.Fields(string(stat[end+1:]))
	const starttime = 22 - 3
	if len(fields) <= starttime {
		return "", false, fmt.Errorf("/proc/%d/stat has no start time", pid)
	}
	if _, err := strconv.ParseUint(fields[starttime], 10, 64); err != nil {
		return "", false, fmt.Errorf("/proc/%d/stat start time %q: %w", pid, fields[starttime], err)
	}
	return strconv.Itoa(pid) + ":" + fields[starttime], true, nil
}

// LaunchedLine is the daemon's log line of a pane it launched (packages/daemon/internal/supervise/machine.go,
// `supervise: launched`, with the claim's owner and the pane's incarnation), which the harness supervisor writes too.
const LaunchedLine = "supervise: launched"

// launchedIncarnations reads, from the daemon log at path after offset (where the driver started the new daemon, whose
// output alone follows it), the incarnation of each pane the daemon logged as launched for owner.
func launchedIncarnations(path string, offset int64, owner string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var launched []string
	for lines.Scan() {
		var line struct {
			Msg         string `json:"msg"`
			Owner       string `json:"owner"`
			Incarnation string `json:"incarnation"`
		}
		if json.Unmarshal(lines.Bytes(), &line) != nil || line.Msg != LaunchedLine || line.Owner != owner || line.Incarnation == "" {
			continue
		}
		launched = append(launched, line.Incarnation)
	}
	return launched, lines.Err()
}

// checkRestart fails the restart unless the new daemon did its work: the item claimed and completed by target after
// the restart (ledger events after EventsFrom), at least one pane of the target seen while it was open, every such
// pane one the new daemon logged as launched, and none of them the old daemon's pane.
func checkRestart(r Restart, events []ledger.Event, target string) []Failure {
	var failures []Failure
	fail := func(format string, args ...any) {
		failures = append(failures, Failure{Kind: FailRestart, Subject: r.Item, Detail: fmt.Sprintf(format, args...)})
	}
	var claimed, completed bool
	for _, event := range events {
		if event.Subject != r.Item {
			continue
		}
		switch event.Kind {
		case "item.claimed", "item.completed":
			if event.ID <= r.EventsFrom {
				fail("%s at ledger event %d, before the restart (event %d)", event.Kind, event.ID, r.EventsFrom)
			}
			if event.Actor != target {
				fail("%s by %s, not by %s", event.Kind, event.Actor, target)
			}
			if event.Kind == "item.claimed" {
				claimed = true
			} else {
				completed = completed || claimed
			}
		}
	}
	if !claimed || !completed {
		fail("not claimed and then completed after the restart (claimed %t, completed %t)", claimed, completed)
	}
	if len(r.Launched) == 0 {
		fail("the new daemon logged no %q of %s", LaunchedLine, target)
	}
	if len(r.WorkPanes) == 0 {
		fail("no pane of %s was seen while the item was open", target)
	}
	for _, pane := range r.WorkPanes {
		switch {
		case pane.Incarnation == r.OldPane.Incarnation:
			fail("the old daemon's pane %s (%s) ran while the item was open", pane.Pane, pane.Incarnation)
		case !slices.Contains(r.Launched, pane.Incarnation):
			fail("the pane %s (%s) ran while the item was open, and the new daemon did not launch it (it launched %v)", pane.Pane, pane.Incarnation, r.Launched)
		}
	}
	return failures
}
