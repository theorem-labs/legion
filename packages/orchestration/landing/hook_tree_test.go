package landing

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The graph and score hooks read the landed tree: the graph hook runs git in "$LAND_TREE", and the score hook runs
// "$LAND_TREE"/tools/score. Through the runner's own executor and real containers, a landed batch with both hooks
// finishes: each finds LAND_TREE at LAND_COMMIT and clean, the score hook reads its previous.json input and leaves
// score.json, and the score apply pushes it once. The hooks' definitions name no mount for the clone: the program
// gives every hook its clone read-only, because LAND_TREE is a worktree inside it.
func TestTheHooksThatReadTheLandedTreeFindIt(t *testing.T) {
	f := newDockerFixture(t, map[string]string{
		"Alpha.lean": "def alpha := 0\n",
		"tools/score": `#!/bin/sh
set -eu
[ "$(git -C "$LAND_TREE" rev-parse HEAD)" = "$1" ] || { echo "tree not at $1" >&2; exit 3; }
[ -z "$(git -C "$LAND_TREE" status --porcelain --untracked-files=all)" ] || { echo "tree not clean" >&2; exit 3; }
[ -r /land/inputs/previous.json ] || { echo "no previous.json" >&2; exit 3; }
printf '{"commit":"%s","part":"%s"}' "$1" "$2" > /land/scratch/score.json
`,
	})
	if err := os.Chmod(filepath.Join(f.repo, "tools", "score"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", "tools/score")
	gitIn(t, f.repo, "commit", "-qm", "executable score")
	landed := gitIn(t, f.repo, "rev-parse", "HEAD")
	d := f.docker
	hooks := []HookConfig{
		{Name: "graph", Apply: ApplyNone, Command: []string{"/bin/sh", "-c",
			`[ "$(git -C "$LAND_TREE" rev-parse HEAD)" = "$LAND_COMMIT" ] && [ -f "$LAND_TREE/Alpha.lean" ] || exit 3`}},
		{Name: "score", Apply: ApplyScore, Outputs: []string{"score.json"},
			Target: &HookTarget{Remote: "https://example.invalid/repo", Ref: "refs/heads/score"},
			Command: []string{"/bin/sh", "-c",
				`exec "$LAND_TREE"/tools/score "$LAND_COMMIT" "$LAND_PART" --tree "$LAND_TREE" --out /land/scratch/score.json --previous /land/inputs/previous.json`}},
	}
	runner := Runner{
		PostLanding:   PostLanding{Parts: []string{"slice"}, Hooks: hooks, TimeoutSeconds: 300, MaxOutputBytes: 1 << 20},
		Container:     Container{Image: d.Image, User: d.User, GitDir: d.GitDir, Mounts: map[string]Mount{}},
		EscalateTo:    "escalations",
		EscalateAfter: 3,
		RetryBackoff:  time.Nanosecond,
		Containers:    d,
		Trees:         d,
	}
	if err := runner.PostLanding.Validate(runner.Container); err != nil {
		t.Fatal(err)
	}
	effects, scores := newEffectLog(), &scoreFake{}
	runner.Effects, runner.Scores = effects, scores
	batch := Batch{ID: "batch", Repository: "r", State: Landed, PullRequest: PullRequest{Number: 7, MergedCommit: landed, State: PullRequestMerged}}
	batch.PostLandEffects = runner.postLandEffects(batch)
	for _, effect := range batch.PostLandEffects {
		effects.create(effect.Key, effect.Op, "", effect.Member, map[string]any{"commit": landed})
		batch.PendingEffects = append(batch.PendingEffects, effect.Key)
	}
	slices.Sort(batch.PendingEffects)

	if err := runner.finishEffects(context.Background(), &batch); err != nil {
		t.Fatalf("finishEffects() error = %v; effects %+v, escalations %q", err, effects.effects, effects.escalations)
	}
	for _, key := range []string{"hook/batch/0/slice", "hook/batch/1/slice"} {
		effect := effects.effects[key]
		if effect.State != EffectDone || effect.Attempts != 0 || !strings.Contains(string(effect.Result), `"exit":0`) {
			t.Fatalf("%s = %+v (result %s), want done at its first run with exit 0", key, effect, effect.Result)
		}
	}
	if got := string(effects.outputs["hook/batch/1/slice"]["score.json"]); got != `{"commit":"`+landed+`","part":"slice"}` {
		t.Fatalf("the score hook left %q", got)
	}
	if len(scores.pushed) != 1 {
		t.Fatalf("the score apply pushed %q, want one path", scores.pushed)
	}
}
