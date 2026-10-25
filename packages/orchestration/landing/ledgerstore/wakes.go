package ledgerstore

import (
	"context"
	"time"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// ReadyWakes wakes a serving program when repository repo gets a migration.ready event. It listens on the ledger's
// event channel on a connection of its own, and on each notification for repo reads the repository's events after
// the last one it saw, so a burst of notifications coalesces into one pending wake. A wake says only that a round
// should run now: the program still reads the ready list itself, and its interval stays the fallback, so a
// notification that is lost (a dropped connection, a restart) delays a batch rather than losing it. The channel is
// closed when ctx ends or the listening connection fails.
func ReadyWakes(ctx context.Context, dsn string, store *ledger.Store, repo string) (<-chan struct{}, error) {
	listener, err := ledger.Listen(ctx, dsn)
	if err != nil {
		return nil, err
	}
	seen, err := store.LatestEventID(ctx, repo)
	if err != nil {
		_ = listener.Close(context.Background())
		return nil, err
	}
	wakes := make(chan struct{}, 1)
	go func() {
		defer close(wakes)
		defer func() { _ = listener.Close(context.Background()) }()
		for {
			changed, err := listener.Wait(ctx)
			if err != nil {
				return
			}
			if changed != repo {
				continue
			}
			events, err := store.Events(ctx, repo, seen, "")
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// A failed read skips this wake; the next notification or the program's interval covers it.
				time.Sleep(100 * time.Millisecond)
				continue
			}
			ready := false
			for _, event := range events {
				seen = max(seen, event.ID)
				ready = ready || event.Kind == "migration.ready"
			}
			if !ready {
				continue
			}
			select {
			case wakes <- struct{}{}:
			default:
			}
		}
	}()
	return wakes, nil
}
