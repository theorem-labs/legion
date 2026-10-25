package landing

import (
	"context"
	"testing"
)

// The event command receives each member's flags from the receipt in
// LAND_FLAGS, and "[]" for a member with none.
func TestLandingEventCarriesTheMemberFlags(t *testing.T) {
	flag := Flag{Check: "statements", Member: "one", Declaration: "Lib.size", Path: "Lib/Use.lean", Line: 9, Reason: "S1: changed"}
	for name, test := range map[string]struct {
		member string
		want   string
	}{
		"flagged":   {member: "one", want: `[{"check":"statements","member":"one","declaration":"Lib.size","path":"Lib/Use.lean","line":9,"reason":"S1: changed"}]`},
		"unflagged": {member: "two", want: `[]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WANT", test.want)
			github := &githubFake{wait: PullRequest{Number: 7, Head: "candidate", MergedCommit: "merged", State: PullRequestMerged}}
			runner := Runner{Store: &storeFake{}, Ledger: &ledgerFake{}, GitHub: github,
				Events: CommandEvents{Command: []string{"sh", "-c", `test "$LAND_FLAGS" = "$WANT"`}}}
			batch := greenBatch()
			batch.FrozenMembers = []Member{member(test.member)}
			batch.ActiveMembers = []Member{member(test.member)}
			batch = rekey(runner, batch)
			batch.Receipt.Checks = []CheckVerdict{{Name: "statements", Verdict: "flag", Flags: []Flag{flag}}}

			landed, err := runner.Publish(context.Background(), batch)
			if err != nil {
				t.Fatalf("Publish() error = %v (LAND_FLAGS is not %s)", err, test.want)
			}
			if landed.State != Landed {
				t.Fatalf("state = %s, want landed", landed.State)
			}
		})
	}
}

// rekey brings a green batch's derived keys to its current members.
func rekey(runner Runner, batch Batch) Batch {
	key := runner.key(batch)
	batch.CandidateKey, batch.ChecksKey = key, key
	for index := range batch.ActiveMembers {
		batch.ActiveMembers[index].LedgerKey = key
		batch.ActiveMembers[index].HashesKey = key
	}
	batch.Receipt = &Receipt{Key: key, Tree: "tree", Members: cloneMembers(batch.ActiveMembers)}
	return batch
}
