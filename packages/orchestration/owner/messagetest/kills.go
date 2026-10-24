package messagetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// DefaultKills is how many owner panes a run kills when Options.Kills is 0: the kill schedule's three moments, then
// before the claim and after the claim once more.
const DefaultKills = 5

// MomentAfterCompletion is where a kill landed that found its item done already: no moment of the step.
const MomentAfterCompletion = "after-completion"

// KillSchedule is the moment of each of n kills: before the claim, after the claim and after the answer, in turn. A
// run kills on this schedule, never by chance, so any n of at least three covers every moment.
func KillSchedule(n int) []string {
	schedule := make([]string, n)
	for index := range schedule {
		schedule[index] = Moments[index%len(Moments)]
	}
	return schedule
}

// Hold is the driver's pause of one item's answer at one moment of its step, and the owner command it holds (PID, in
// HeldPath). After the claim, `message-test answer` holds with the item claimed and nothing recorded; after the
// answer, it holds inside the step runner's completion (owner.Runner.Run calls Ledger.Complete last, after the record
// commit and the follow-ups, owner/step.go), with the record committed and the item still open. So a kill at either
// moment lands there, whatever the owner model's pace.
type Hold struct {
	Item   string `json:"item"`
	Moment string `json:"moment"`
	PID    int    `json:"pid,omitempty"`
}

// HoldPath is the hold the driver puts in the test deployment's state directory.
func HoldPath(stateDir string) string { return filepath.Join(stateDir, "message-test", "hold.json") }

// HeldPath is where the owner command that a hold paused says so.
func HeldPath(stateDir string) string { return filepath.Join(stateDir, "message-test", "held.json") }

// holdPoll is how often a held owner command looks whether its hold is released.
const holdPoll = 50 * time.Millisecond

// PutHold pauses the answer of item at moment, after the claim or after the answer, until ReleaseHold.
func PutHold(stateDir, item, moment string) error {
	if moment != MomentAfterClaim && moment != MomentAfterAnswer {
		return fmt.Errorf("a hold pauses an answer after the claim or after the answer, not %q", moment)
	}
	return writeJSON(HoldPath(stateDir), Hold{Item: item, Moment: moment})
}

// ReleaseHold ends the hold, and the held command's record of it.
func ReleaseHold(stateDir string) error {
	return errors.Join(removeIfThere(HoldPath(stateDir)), removeIfThere(HeldPath(stateDir)))
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadHold reads the driver's hold, when it holds an answer.
func ReadHold(stateDir string) (Hold, bool, error) { return readHold(HoldPath(stateDir)) }

// ReadHeld reads the owner command a hold paused, when one says so.
func ReadHeld(stateDir string) (Hold, bool, error) {
	return readHold(HeldPath(stateDir))
}

func readHold(path string) (Hold, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Hold{}, false, nil
	}
	if err != nil {
		return Hold{}, false, err
	}
	var hold Hold
	if err := json.Unmarshal(data, &hold); err != nil {
		// A file still being written: no hold yet.
		return Hold{}, false, nil
	}
	return hold, true, nil
}

// AwaitRelease pauses the caller while the driver's hold names item at moment: it writes HeldPath with its own PID
// and waits until the hold is gone, or ctx ends (the pane killed), with ctx's error. With no such hold it returns at
// once. held says whether it paused.
func AwaitRelease(ctx context.Context, stateDir, item, moment string) (held bool, err error) {
	hold, found, err := readHold(HoldPath(stateDir))
	if err != nil || !found || hold.Item != item || hold.Moment != moment {
		return false, err
	}
	if err := writeJSON(HeldPath(stateDir), Hold{Item: item, Moment: moment, PID: os.Getpid()}); err != nil {
		return true, err
	}
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(holdPoll):
		}
		if _, found, err := readHold(HoldPath(stateDir)); err != nil || !found {
			return true, err
		}
	}
}

// observedMoment is the moment a kill landed at, by the item's state just after it: done (after the completion), its
// record committed (after the answer), claimed by the owner (after the claim), or neither (before the claim).
func observedMoment(owner, holder, record string, done bool) string {
	switch {
	case done:
		return MomentAfterCompletion
	case record != "":
		return MomentAfterAnswer
	case holder == owner:
		return MomentAfterClaim
	}
	return MomentBeforeClaim
}

// checkKills fails each kill that landed at another moment than the schedule set for it, and each moment of the
// schedule that no kill landed at: a run must show a kill before the claim, after the claim, and between the answer's
// record and its completion.
func checkKills(schedule []string, kills []Kill) []Failure {
	var failures []Failure
	for index, kill := range kills {
		if kill.Observed != kill.Scheduled {
			failures = append(failures, Failure{Kind: FailKill, Subject: kill.Item,
				Detail: fmt.Sprintf("kill %d was scheduled %s and landed %s (holder %q, record %q, done %t)", index+1, kill.Scheduled, kill.Observed, kill.Holder, kill.Record, kill.Done)})
		}
	}
	for _, moment := range Moments {
		if slices.Contains(schedule, moment) && !slices.ContainsFunc(kills, func(kill Kill) bool { return kill.Observed == moment }) {
			failures = append(failures, Failure{Kind: FailKill, Subject: moment, Detail: "no kill landed at this moment of the step"})
		}
	}
	return failures
}
