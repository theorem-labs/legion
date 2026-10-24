package messagetest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// A pane's incarnation is the daemon's: its root pid and that process's start ticks (field 22 of /proc/<pid>/stat,
// counted after the command name), so a pid the kernel gives to another process is another incarnation.
func TestProcessIncarnationIsThePIDAndItsStartTicks(t *testing.T) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("no /proc")
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	want := fmt.Sprintf("%d:%s", os.Getpid(), fields[22-3])
	if got, alive, err := ProcessIncarnation(os.Getpid()); err != nil || !alive || got != want {
		t.Fatalf("ProcessIncarnation(self) = %q, %t, %v; want %q", got, alive, err, want)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if got, alive, err := ProcessIncarnation(gone.Process.Pid); err != nil || alive {
		t.Fatalf("ProcessIncarnation of an exited process = %q, %t, %v; want not alive", got, alive, err)
	}
}

// The new daemon's launches are its own `supervise: launched` lines, after the offset of its start in the log: the
// old daemon's lines before it, another owner's launches and any other line are not.
func TestLaunchedIncarnationsReadsTheNewDaemonsLaunchesOfOneOwner(t *testing.T) {
	old := `{"time":"2026-10-24T08:05:28Z","level":"INFO","msg":"supervise: launched","owner":"struct:t","generation":1,"incarnation":"11:100","resumed":false}` + "\n"
	fresh := "--- message-test 2026-10-24T08:09:00Z: legion start --config legion.yaml\n" +
		`{"time":"2026-10-24T08:09:01Z","level":"WARN","msg":"supervise: process died","owner":"struct:t","incarnation":"11:100"}` + "\n" +
		"not a JSON line\n" +
		`{"time":"2026-10-24T08:09:02Z","level":"INFO","msg":"supervise: launched","owner":"function:f@x/y.c","incarnation":"33:300"}` + "\n" +
		`{"time":"2026-10-24T08:09:03Z","level":"INFO","msg":"supervise: launched","owner":"struct:t","generation":7,"incarnation":"22:200","resumed":true}` + "\n"
	path := filepath.Join(t.TempDir(), "legion.log")
	if err := os.WriteFile(path, []byte(old+fresh), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := launchedIncarnations(path, int64(len(old)), "struct:t")
	if err != nil || !slices.Equal(got, []string{"22:200"}) {
		t.Fatalf("launchedIncarnations = %v, %v; want only the new daemon's launch of struct:t", got, err)
	}
}

// The restart passes only when the new daemon did the work: the item claimed and completed by the target after the
// restart, while every target pane the driver saw was one the new daemon launched, and the old daemon's pane none.
func TestCheckRestartRequiresTheItemDoneByAPaneTheNewDaemonLaunched(t *testing.T) {
	const st, need = "struct:t", "need:function:f@x/y.c->struct:t@r9"
	passing := Restart{Item: need, EventsFrom: 10, OldPane: PaneProcess{Pane: "%2", PID: 11, Incarnation: "11:100"},
		Launched: []string{"22:200"}, WorkPanes: []PaneProcess{{Pane: "%5", PID: 22, Incarnation: "22:200"}}}
	events := []ledger.Event{
		{ID: 11, Kind: "item.enqueued", Subject: need, Actor: st},
		{ID: 12, Kind: "item.claimed", Subject: need, Actor: st},
		{ID: 13, Kind: "item.completed", Subject: need, Actor: st},
	}
	if failures := checkRestart(passing, events, st); len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}
	for name, test := range map[string]struct {
		restart Restart
		events  []ledger.Event
	}{
		"the old daemon's pane did the work": {restart: with(passing, func(r *Restart) { r.WorkPanes = []PaneProcess{r.OldPane} }), events: events},
		"a pane the new daemon did not launch": {restart: with(passing, func(r *Restart) {
			r.WorkPanes = append(r.WorkPanes, PaneProcess{Pane: "%6", PID: 33, Incarnation: "33:300"})
		}), events: events},
		"no pane of the target seen":   {restart: with(passing, func(r *Restart) { r.WorkPanes = nil }), events: events},
		"the new daemon launched none": {restart: with(passing, func(r *Restart) { r.Launched = nil }), events: events},
		"claimed before the restart":   {restart: passing, events: append([]ledger.Event{{ID: 9, Kind: "item.claimed", Subject: need, Actor: st}}, events[2])},
		"never completed":              {restart: passing, events: events[:2]},
		"claimed by another holder": {restart: passing, events: []ledger.Event{events[0],
			{ID: 12, Kind: "item.claimed", Subject: need, Actor: "function:f@x/y.c"}, {ID: 13, Kind: "item.completed", Subject: need, Actor: "function:f@x/y.c"}}},
	} {
		failures := checkRestart(test.restart, test.events, st)
		if len(failures) == 0 || !slices.ContainsFunc(failures, func(f Failure) bool { return f.Kind == FailRestart && f.Subject == need }) {
			t.Errorf("%s: failures = %+v, want a restart failure of %s", name, failures, need)
		}
	}
}

func with(r Restart, change func(*Restart)) Restart {
	r.Launched, r.WorkPanes = slices.Clone(r.Launched), slices.Clone(r.WorkPanes)
	change(&r)
	return r
}
