package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/replay"
)

// writeFile writes contents to dir/name.
func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	write(t, filepath.Join(dir, name), contents)
}

// writeManifest writes a replay manifest into dir and returns its path.
func writeManifest(t *testing.T, dir string, manifest replay.Manifest) string {
	t.Helper()
	manifest.Format = replay.Format
	if manifest.Name == "" {
		manifest.Name = "t"
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	write(t, path, string(raw))
	return path
}

func readSummary(t *testing.T, path string) []RoundSummary {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rounds []RoundSummary
	if err := json.Unmarshal(body, &rounds); err != nil {
		t.Fatalf("summary %s: %v", body, err)
	}
	return rounds
}

// row finds the replay row with these first three fields. The last line's
// empty outcome is trimmed with the output's trailing newline, so a row of
// seven fields is padded back to eight.
func row(rows []string, round, member, source string) ([]string, bool) {
	for _, line := range rows {
		fields := strings.Split(line, "\t")
		if len(fields) == 7 {
			fields = append(fields, "")
		}
		if len(fields) == 8 && fields[0] == round && fields[1] == member && fields[2] == source {
			return fields, true
		}
	}
	return nil, false
}

func replayConfig(t *testing.T, repo string, checks ...map[string]any) string {
	t.Helper()
	if checks == nil {
		checks = []map[string]any{}
	}
	return writeConfig(t, map[string]any{"repository": map[string]any{"name": "r", "path": repo, "git_dir": "/unused"}, "moduleRoots": []string{"Alpha"}, "checks": checks})
}

func graphCheck(t *testing.T, lines string) map[string]any {
	return map[string]any{"name": "names", "command": []string{writeScript(t, "printf '"+lines+"'")}, "format": "graph"}
}

type replayRun struct {
	rows    []string
	summary []RoundSummary
	err     error
}

func runReplay(t *testing.T, manifest, config string, extra ...string) replayRun {
	t.Helper()
	summary := filepath.Join(t.TempDir(), "summary.json")
	var out bytes.Buffer
	err := run(context.Background(), append([]string{"replay", "--manifest", manifest, "--config", config, "--summary", summary}, extra...), &out)
	return replayRun{rows: strings.Split(strings.TrimSpace(out.String()), "\n"), summary: readSummary(t, summary), err: err}
}

const goneLog = "✖ [1/1] Building Alpha.Use\nerror: Alpha/Use.lean:2:12: Unknown identifier `gone`\n"

func TestLandReplayPrintsChecksAndBuildBlamePerRound(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0", "other": "+def extra := 1"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", goneLog)
	writeFile(t, dir, "build-round-2.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members: []replay.Member{{ID: "culprit", Head: heads["culprit"]}, {ID: "other", Head: heads["other"]}},
		BuildRounds: []replay.BuildRound{
			{Round: 1, Log: "build-round-1.log", RecordedKicks: []replay.Kick{{Member: "culprit", Reason: "ident:gone"}}},
			{Round: 2, Excluded: []string{"culprit"}, Log: "build-round-2.log"},
		}})
	config := replayConfig(t, repo, graphCheck(t, `refuse\tunknown\tAlpha.gone\tAlpha/Use.lean:2:12\tN1: x\n`))
	got := runReplay(t, manifest, config)
	if got.err != nil {
		t.Fatalf("replay: %v, rows %q", got.err, got.rows)
	}
	refusal, found := row(got.rows, "1", "culprit", "check-refuse")
	if !found || refusal[3] != "N1" || refusal[4] != "Alpha.gone" || refusal[5] != "unknown" || refusal[6] != "removed" || refusal[7] != "explained-by-log" {
		t.Fatalf("rows = %q", got.rows)
	}
	if build, found := row(got.rows, "1", "culprit", "build"); !found || build[3] != "Alpha.Use" || build[4] != "gone" {
		t.Fatalf("rows = %q, want the build blame of culprit", got.rows)
	}
	if kick, found := row(got.rows, "1", "culprit", "recorded"); !found || kick[3] != "ident" || kick[4] != "gone" {
		t.Fatalf("rows = %q, want the recorded kick", got.rows)
	}
	if _, found := row(got.rows, "2", "culprit", "check-refuse"); found {
		t.Fatalf("rows = %q: round 2 excludes culprit", got.rows)
	}
	if len(got.summary) != 2 {
		t.Fatalf("summary = %+v", got.summary)
	}
	first := got.summary[0]
	if first.Round != 1 || first.Members != 2 || len(first.Refused) != 1 || first.Refused[0].Outcome != "explained-by-log" ||
		len(first.BuildNamed) != 1 || len(first.RecordedNameKicks) != 1 || len(first.Missed) != 0 || len(first.Unable) != 0 {
		t.Fatalf("round 1 summary = %+v", first)
	}
	if _, timed := first.CheckSeconds["names"]; !timed {
		t.Fatalf("round 1 summary has no seconds for names: %+v", first.CheckSeconds)
	}
	// Round 2 excludes culprit, so no member's evidence names Alpha.gone and
	// the names check is unable there.
	if second := got.summary[1]; second.Members != 1 || len(second.Refused) != 0 || len(second.Unable) != 1 || second.Unable[0].Check != "names" {
		t.Fatalf("round 2 summary = %+v", second)
	}
}

func TestLandReplayRoundSelectsOneRound(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", goneLog)
	writeFile(t, dir, "build-round-2.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members:     []replay.Member{{ID: "culprit", Head: heads["culprit"]}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}, {Round: 2, Log: "build-round-2.log"}}})
	got := runReplay(t, manifest, replayConfig(t, repo), "--round", "2")
	if got.err != nil || len(got.summary) != 1 || got.summary[0].Round != 2 {
		t.Fatalf("replay: %v, summary %+v", got.err, got.summary)
	}
}

func TestLandReplayConfirmsAnUnexplainedRefusal(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"quiet": "+def extra := 1"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members:     []replay.Member{{ID: "quiet", Head: heads["quiet"]}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}}})
	config := replayConfig(t, repo, graphCheck(t, `refuse\tadded\tAlpha.extra\tAlpha/Core.lean:5:1\tN2: x\n`))
	modules := filepath.Join(t.TempDir(), "modules")
	got := runReplay(t, manifest, config, "--confirm-with", `test -f Alpha/Core.lean && echo "$LAND_MODULES" >> `+modules)
	if got.err != nil {
		t.Fatalf("replay: %v", got.err)
	}
	rounds := got.summary
	if len(rounds[0].FalseRefusals) != 1 || rounds[0].FalseRefusals[0].Outcome != "confirmed-green" || rounds[0].Refused[0].Outcome != "confirmed-green" {
		t.Fatalf("summary = %+v", rounds)
	}
	if body, _ := os.ReadFile(modules); strings.TrimSpace(string(body)) != "Alpha.Core" {
		t.Fatalf("confirmation built %q, want Alpha.Core", body)
	}
}

func TestLandReplayRecordsARedConfirmationAndInfrastructure(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"quiet": "+def extra := 1"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members:     []replay.Member{{ID: "quiet", Head: heads["quiet"]}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}}})
	config := replayConfig(t, repo, graphCheck(t, `refuse\tadded\tAlpha.extra\tAlpha/Core.lean:5:1\tN2: x\n`))
	red := runReplay(t, manifest, config, "--confirm-with", "exit 1")
	if red.err != nil || red.summary[0].Refused[0].Outcome != "confirmed-red" || len(red.summary[0].FalseRefusals) != 0 {
		t.Fatalf("red: %v, summary %+v", red.err, red.summary)
	}
	infra := runReplay(t, manifest, config, "--confirm-with", "kill -9 $$")
	if infra.err == nil || infra.summary[0].Refused[0].Outcome != "unconfirmed-infra" {
		t.Fatalf("infra: %v, summary %+v; want unconfirmed-infra and an error", infra.err, infra.summary)
	}
}

func TestLandReplayPrintsAnUnattributedFinding(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"quiet": "+def extra := 1"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members:     []replay.Member{{ID: "quiet", Head: heads["quiet"]}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}}})
	config := replayConfig(t, repo, graphCheck(t, `refuse\tunknown\tAlpha.gone\tAlpha/Use.lean:2:12\tN1: x\n`))
	got := runReplay(t, manifest, config)
	unattributed, found := row(got.rows, "1", "", "check-unattributed")
	if got.err != nil || !found || unattributed[4] != "Alpha.gone" || unattributed[6] != "no evidence" {
		t.Fatalf("replay: %v, rows %q", got.err, got.rows)
	}
	if unable := got.summary[0].Unable; len(unable) != 1 || unable[0].Check != "names" {
		t.Fatalf("unable = %+v", unable)
	}
}

// Each build-named member that no check refused or flagged is a miss, with
// its cause; a green confirmation makes it a wrong build blame instead.
func TestLandReplayClassifiesTheMisses(t *testing.T) {
	for name, test := range map[string]struct {
		members    map[string]string
		log        string
		unresolved []replay.Unresolved
		kicks      []replay.Kick
		check      string
		confirm    string
		missed     []SummaryMiss
		blameWrong []string
	}{
		"defect": {
			members: map[string]string{"culprit": "-def gone := 0"}, log: goneLog,
			missed: []SummaryMiss{{Member: "culprit", Name: "gone", Cause: "defect"}},
		},
		"confirmed red": {
			members: map[string]string{"culprit": "-def gone := 0"}, log: goneLog, confirm: "exit 1",
			missed: []SummaryMiss{{Member: "culprit", Name: "gone", Cause: "defect", Confirmation: "confirmed-red"}},
		},
		"blame wrong": {
			members: map[string]string{"culprit": "-def gone := 0"}, log: goneLog, confirm: "exit 0",
			blameWrong: []string{"culprit"},
		},
		"a flag covers the name": {
			members: map[string]string{"culprit": "-def gone := 0"}, log: goneLog,
			check: `flag\tunknown\tAlpha.gone\tAlpha/Use.lean:2:12\tN3: x\n`,
		},
		"use added in the same batch": {
			members: map[string]string{"culprit": "-def gone := 0", "user": "+def again := gone"},
			log:     "✖ [1/1] Building Alpha.Core\nerror: Alpha/Core.lean:4:14: Unknown identifier `gone`\n",
			missed:  []SummaryMiss{{Member: "culprit", Name: "gone", Cause: "use added in the same batch"}},
		},
		"not a name error": {
			members: map[string]string{"editor": "+  + 2"},
			log:     "✖ [1/1] Building Alpha.Use\nerror: Alpha/Use.lean:2:12: application type mismatch\n  in the application\n    keep\n",
			missed:  []SummaryMiss{{Member: "editor", Name: "keep", Cause: "not a name error"}},
		},
		"generated name": {
			members: map[string]string{"opener": "-namespace Alpha"},
			log:     "✖ [1/1] Building Alpha.Use\nerror: Alpha/Use.lean:2:12: Unknown identifier `Alpha.instReprThing`\n",
			missed:  []SummaryMiss{{Member: "opener", Name: "Alpha.instReprThing", Cause: "generated name"}},
		},
		"unresolved member": {
			members: map[string]string{}, log: "",
			unresolved: []replay.Unresolved{{ID: "lost", Why: "its head is gone"}},
			kicks:      []replay.Kick{{Member: "lost", Reason: "ident:Alpha.gone"}},
			missed:     []SummaryMiss{{Member: "lost", Name: "Alpha.gone", Cause: "unresolved member"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo, base, heads := scratchRepository(t, test.members)
			dir := t.TempDir()
			writeFile(t, dir, "build-round-1.log", test.log)
			var members []replay.Member
			for _, id := range []string{"culprit", "user", "editor", "opener"} {
				if head, found := heads[id]; found {
					members = append(members, replay.Member{ID: id, Head: head})
				}
			}
			manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base, Members: members, Unresolved: test.unresolved,
				BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log", RecordedKicks: test.kicks}}})
			var checks []map[string]any
			if test.check != "" {
				checks = append(checks, graphCheck(t, test.check))
			}
			var extra []string
			if test.confirm != "" {
				extra = []string{"--confirm-with", test.confirm}
			}
			got := runReplay(t, manifest, replayConfig(t, repo, checks...), extra...)
			if got.err != nil {
				t.Fatalf("replay: %v, rows %q", got.err, got.rows)
			}
			round := got.summary[0]
			if !reflect.DeepEqual(round.Missed, test.missed) && (len(round.Missed) != 0 || len(test.missed) != 0) {
				t.Fatalf("missed = %+v, want %+v (build-named %+v)", round.Missed, test.missed, round.BuildNamed)
			}
			var wrong []string
			for _, build := range round.BlameWrong {
				wrong = append(wrong, build.Member)
			}
			if !reflect.DeepEqual(wrong, test.blameWrong) {
				t.Fatalf("blame wrong = %v, want %v", wrong, test.blameWrong)
			}
		})
	}
}

// The recorded program joins the reasons of one kick with "; ": a name kick
// that also had a red build ("ident:X; red", "clash; red") is still a name
// kick, and its row names the name and every reason.
func TestLandReplayReadsCombinedKickReasons(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0", "clasher": "+def extra := 1", "red": "+def other := 2"})
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "")
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base,
		Members: []replay.Member{{ID: "culprit", Head: heads["culprit"]}, {ID: "clasher", Head: heads["clasher"]}, {ID: "red", Head: heads["red"]}},
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log", RecordedKicks: []replay.Kick{
			{Member: "culprit", Reason: "ident:Alpha.gone; red"},
			{Member: "clasher", Reason: "clash; red"},
			{Member: "red", Reason: "red"},
		}}}})
	got := runReplay(t, manifest, replayConfig(t, repo))
	if got.err != nil {
		t.Fatalf("replay: %v", got.err)
	}
	if kick, found := row(got.rows, "1", "culprit", "recorded"); !found || kick[3] != "ident; red" || kick[4] != "Alpha.gone" {
		t.Fatalf("rows = %q, want culprit's kick as reasons \"ident; red\" and name Alpha.gone", got.rows)
	}
	if kick, found := row(got.rows, "1", "clasher", "recorded"); !found || kick[3] != "clash; red" || kick[4] != "" {
		t.Fatalf("rows = %q, want clasher's kick as reasons \"clash; red\" and no name", got.rows)
	}
	var kicked []string
	for _, kick := range got.summary[0].RecordedNameKicks {
		kicked = append(kicked, kick.Member)
	}
	if !reflect.DeepEqual(kicked, []string{"culprit", "clasher"}) {
		t.Fatalf("recorded name kicks = %v, want culprit and clasher", kicked)
	}
}

// Only the members that changed a file are asked what they added to it, so a
// round costs a diff per member and changed file, not per member and error.
// Here one member changed Alpha/Core.lean and six did not, and the six are
// never asked, which a git wrapper on PATH counts.
func TestLandReplayAsksOnlyTheMembersThatChangedTheFile(t *testing.T) {
	repo, base, heads := scratchRepository(t, map[string]string{"culprit": "-def gone := 0"})
	// Six members that change another file, so only the culprit can have
	// added a line of Alpha/Core.lean.
	for _, id := range []string{"q1", "q2", "q3", "q4", "q5", "q6"} {
		command(t, repo, "checkout", "-q", "-b", id, base)
		write(t, filepath.Join(repo, "Alpha", id+".lean"), "def "+id+" := 1\n")
		command(t, repo, "add", ".")
		command(t, repo, "commit", "-qm", id)
		heads[id] = command(t, repo, "rev-parse", "HEAD")
	}
	command(t, repo, "checkout", "-q", base)
	dir := t.TempDir()
	writeFile(t, dir, "build-round-1.log", "✖ [1/1] Building Alpha.Core\nerror: Alpha/Core.lean:4:14: Unknown identifier `gone`\n")
	var manifestMembers []replay.Member
	for id, head := range heads {
		manifestMembers = append(manifestMembers, replay.Member{ID: id, Head: head})
	}
	sort.Slice(manifestMembers, func(i, j int) bool { return manifestMembers[i].ID < manifestMembers[j].ID })
	manifest := writeManifest(t, dir, replay.Manifest{Repository: repo, Base: base, Members: manifestMembers,
		BuildRounds: []replay.BuildRound{{Round: 1, Log: "build-round-1.log"}}})
	counts := filepath.Join(t.TempDir(), "diffs")
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(path, "git"), "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = diff ] && echo \"$@\" >> "+counts+"; done\nexec "+real+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(path, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", path+":"+os.Getenv("PATH"))
	if got := runReplay(t, manifest, replayConfig(t, repo)); got.err != nil {
		t.Fatalf("replay: %v", got.err)
	}
	body, _ := os.ReadFile(counts)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		// The per-path diff of the miss classification, not the evidence
		// step's diff of every member's Lean files.
		if strings.Contains(line, "-- Alpha/Core.lean") && !strings.Contains(line, heads["culprit"]) {
			t.Fatalf("a member that did not change the file was asked: git %s", line)
		}
	}
}
