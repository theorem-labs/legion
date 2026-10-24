package messagetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every full run kills by a fixed schedule, never by chance: before the claim, after the claim and after the answer,
// in turn, so any count of at least three covers every moment.
func TestKillScheduleKillsAtEachMomentInTurn(t *testing.T) {
	if got := KillSchedule(5); !slices.Equal(got, []string{MomentBeforeClaim, MomentAfterClaim, MomentAfterAnswer, MomentBeforeClaim, MomentAfterClaim}) {
		t.Fatalf("KillSchedule(5) = %v", got)
	}
	if got := KillSchedule(3); !slices.Equal(got, Moments) {
		t.Fatalf("KillSchedule(3) = %v, want %v", got, Moments)
	}
}

// In the Run API no kill count (0) means the default schedule, which covers every moment; the report and report.md
// show it. An explicit count of one or two is refused: such a run could not show every moment.
func TestRunWithNoKillCountKillsOnTheDefaultSchedule(t *testing.T) {
	opts := setupStopOptions(t, "")
	opts.Kills = 0
	report, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := KillSchedule(DefaultKills); DefaultKills < len(Moments) || !slices.Equal(report.KillSchedule, want) {
		t.Fatalf("the kill schedule of a run with no kill count = %v, want %v", report.KillSchedule, want)
	}
	markdown, err := os.ReadFile(filepath.Join(opts.Out, "report.md"))
	if err != nil || !strings.Contains(string(markdown), "- Kill schedule: `before-claim`, `after-claim`, `after-answer`, `before-claim`, `after-claim`") {
		t.Fatalf("report.md does not show the kill schedule: %v\n%s", err, markdown)
	}
}

// A hold pauses one item's answer at one moment until the driver releases it: the owner command writes who it is
// (HeldPath) and waits. Any other item or moment goes on at once, and a cancelled command (a killed pane) ends the
// wait with its context's error.
func TestAwaitReleaseHoldsOnlyTheHeldItemAtItsMoment(t *testing.T) {
	state := t.TempDir()
	item := "need:function:a->struct:t@r4"
	if held, err := AwaitRelease(t.Context(), state, item, MomentAfterAnswer); held || err != nil {
		t.Fatalf("AwaitRelease with no hold = %t, %v", held, err)
	}
	if err := PutHold(state, item, MomentAfterAnswer); err != nil {
		t.Fatal(err)
	}
	for _, other := range []Hold{{Item: "need:function:a->struct:t@r5", Moment: MomentAfterAnswer}, {Item: item, Moment: MomentAfterClaim}} {
		if held, err := AwaitRelease(t.Context(), state, other.Item, other.Moment); held || err != nil {
			t.Fatalf("AwaitRelease of %+v under the hold of %s = %t, %v", other, item, held, err)
		}
	}
	type result struct {
		held bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		held, err := AwaitRelease(t.Context(), state, item, MomentAfterAnswer)
		done <- result{held, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held, found, err := ReadHeld(state)
		if err != nil {
			t.Fatal(err)
		}
		if found && held == (Hold{Item: item, Moment: MomentAfterAnswer, PID: os.Getpid()}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held command never said so: %+v, %t", held, found)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("AwaitRelease returned %+v before the release", r)
	case <-time.After(200 * time.Millisecond):
	}
	if err := ReleaseHold(state); err != nil {
		t.Fatal(err)
	}
	if r := <-done; !r.held || r.err != nil {
		t.Fatalf("AwaitRelease after the release = %+v, want held and no error", r)
	}
	if _, found, err := ReadHeld(state); found || err != nil {
		t.Fatalf("the held record outlived the release: %t, %v", found, err)
	}

	if err := PutHold(state, item, MomentAfterClaim); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if held, err := AwaitRelease(ctx, state, item, MomentAfterClaim); !held || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AwaitRelease of a cancelled command = %t, %v, want held and its context's error", held, err)
	}
}

// The run fails unless a kill landed at each moment of the schedule, each kill at the moment the schedule set: an
// after-answer kill that found its item done landed after the completion, not between the record and the completion.
func TestCheckKillsFailsAMomentNoKillLandedAt(t *testing.T) {
	schedule := KillSchedule(3)
	landed := []Kill{
		{Item: "need:e@r4", Scheduled: MomentBeforeClaim, Observed: MomentBeforeClaim},
		{Item: "need:e@r5", Scheduled: MomentAfterClaim, Observed: MomentAfterClaim, Holder: "struct:t"},
		{Item: "need:e@r6", Scheduled: MomentAfterAnswer, Observed: MomentAfterAnswer, Holder: "struct:t", Record: "0a1b2c"},
	}
	if failures := checkKills(schedule, landed); len(failures) != 0 {
		t.Fatalf("failures = %+v", failures)
	}

	late := slices.Clone(landed)
	late[2].Observed, late[2].Done = MomentAfterCompletion, true
	failures := checkKills(schedule, late)
	if len(failures) != 2 || !slices.ContainsFunc(failures, func(f Failure) bool { return f.Kind == FailKill && f.Subject == "need:e@r6" }) ||
		!slices.ContainsFunc(failures, func(f Failure) bool { return f.Kind == FailKill && f.Subject == MomentAfterAnswer }) {
		t.Fatalf("an after-answer kill that landed after the completion: failures = %+v, want the kill and the moment it missed", failures)
	}

	failures = checkKills(schedule, landed[:2])
	if len(failures) != 1 || failures[0].Kind != FailKill || failures[0].Subject != MomentAfterAnswer {
		t.Fatalf("a run with no after-answer kill: failures = %+v", failures)
	}
}
