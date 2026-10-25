package landing

import (
	"context"
	"testing"
)

func TestReviewCommandEventsReceivesTheMergedCommit(t *testing.T) {
	events := CommandEvents{Directory: t.TempDir(), Command: []string{"sh", "-c", "test \"$LAND_COMMIT\" = merged && test \"$LAND_MEMBER\" = member"}}
	if err := events.Publish(context.Background(), LandingEvent{Batch: "batch", Member: "member", Commit: "merged"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
}
