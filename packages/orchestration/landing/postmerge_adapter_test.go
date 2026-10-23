package landing

import (
	"context"
	"testing"
)

func TestReviewCommandScorerAndEventsReceiveMergedCommit(t *testing.T) {
	scorer := CommandScorer{
		Directory:     t.TempDir(),
		ScoreCommand:  []string{"sh", "-c", "printf score-$LAND_COMMIT"},
		CommitCommand: []string{"sh", "-c", "test \"$LAND_COMMIT\" = merged && test \"$LAND_SCORE_OUTPUT\" = score-merged"},
	}
	output, err := scorer.Score(context.Background(), "merged", "batch")
	if err != nil {
		t.Fatalf("Score() error = %v", err)
	}
	if output != "score-merged" {
		t.Fatalf("Score() = %q, want score-merged", output)
	}
	if err := scorer.Commit(context.Background(), "repo", "merged", "scores/out", output); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	events := CommandEvents{Directory: t.TempDir(), Command: []string{"sh", "-c", "test \"$LAND_COMMIT\" = merged && test \"$LAND_MEMBER\" = member"}}
	if err := events.Publish(context.Background(), LandingEvent{Batch: "batch", Member: "member", Commit: "merged"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}
