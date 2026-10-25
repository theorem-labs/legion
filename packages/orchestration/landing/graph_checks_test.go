package landing

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
)

type graphCheckerFake struct {
	name  string
	lines []string
	saw   []CheckInput
}

func (g *graphCheckerFake) Name() string { return g.name }
func (g *graphCheckerFake) Check(_ context.Context, in CheckInput) ([]Finding, error) {
	g.saw = append(g.saw, in)
	var out []Finding
	for _, line := range g.lines {
		f, err := ParseGraphFinding(line)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func green(trees ...string) *builderFake {
	results := map[string]blame.BuildResult{}
	for _, tree := range trees {
		results[tree] = blame.BuildResult{Green: true}
	}
	return &builderFake{results: results}
}

func changes(files ...string) gitx.Change { return gitx.Change{Files: files} }

// privatised is the evidence of a member that made name private.
func privatised(name string) blame.Evidence {
	return blame.IndexDeclarations([]blame.Declaration{{File: "Core.lean", Name: name, Text: "def " + name + " := 0"}},
		[]blame.Declaration{{File: "Core.lean", Name: name, Text: "private def " + name + " := 0", Private: true}})
}

// edited is the evidence of a member that edited the body of name.
func edited(name string) blame.Evidence {
	evidence := blame.NewEvidence()
	evidence.Inside[blame.DeclarationID{File: "Core.lean", Name: name}] = blame.Declaration{File: "Core.lean", Name: name}
	return evidence
}

func droppedIDs(batch Batch) []string {
	var out []string
	for _, d := range batch.Dropped {
		out = append(out, d.Migration)
	}
	sort.Strings(out)
	return out
}

func checkVerdict(batch Batch, name string) CheckVerdict {
	for _, v := range batch.Checks {
		if v.Name == name {
			return v
		}
	}
	return CheckVerdict{}
}

func TestGraphCheckReturnsOnlyTheMemberWhoseEvidenceNamesTheDeclaration(t *testing.T) {
	ledger := &ledgerFake{}
	runner := testRunner(ledger, green("tree-bystander"))
	check := &graphCheckerFake{name: "names", lines: []string{
		"refuse\tunknown\tLib.helper\tLib/Use.lean:4:3\tN1: Lib/Core.lean no longer declares Lib.helper publicly",
	}}
	runner.Checks = []Checker{check}
	git := runner.Git.(*gitFake)
	git.changes["culprit"] = changes("Core.lean")
	git.changes["bystander"] = changes("Core.lean")
	git.evidence["culprit"] = privatised("Lib.helper")
	git.evidence["bystander"] = edited("Lib.other")

	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base",
		Members: []Member{member("culprit"), member("bystander")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := droppedIDs(batch); !reflect.DeepEqual(got, []string{"culprit"}) {
		t.Fatalf("dropped = %v, want [culprit]", got)
	}
	if want := []returned{{migration: "culprit", reason: "check names: N1 Lib.helper (privatised)"}}; !sameReturned(ledger.returned, want) {
		t.Fatalf("returned = %+v, want %+v", ledger.returned, want)
	}
	if check.saw[0].Base != "base" || check.saw[0].Tree != "tree-culprit+bystander" {
		t.Fatalf("the check saw %+v", check.saw[0])
	}
	if batch.State != Green {
		t.Fatalf("state %s (%s), want green after the culprit left", batch.State, batch.Reason)
	}
}

func TestGraphCheckRefusalListsTheMemberAndItsEvidence(t *testing.T) {
	runner := testRunner(&ledgerFake{}, green())
	runner.Checks = []Checker{&graphCheckerFake{name: "names", lines: []string{
		"refuse\tunknown\tLib.helper\tLib/Use.lean:4:3\tN1: Lib/Core.lean no longer declares Lib.helper publicly",
	}}}
	git := runner.Git.(*gitFake)
	git.evidence["culprit"] = privatised("Lib.helper")
	git.evidence["bystander"] = edited("Lib.other")
	batch := Batch{ID: "b", Repository: "repo", Base: "base", Candidate: gitx.Result{Tree: "tree"},
		ActiveMembers: []Member{member("culprit"), member("bystander")}}
	verdicts, culprits, err := runner.evaluateChecks(context.Background(), &batch, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []Refusal{{Member: "culprit", Declaration: "Lib.helper", Kind: blame.Unknown, Path: "Lib/Use.lean", Line: 4, Strength: 3, Evidence: "privatised",
		Reason: "N1: Lib/Core.lean no longer declares Lib.helper publicly"}}
	if len(verdicts) != 1 || verdicts[0].Verdict != "fail" || !reflect.DeepEqual(verdicts[0].Refused, want) {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if !reflect.DeepEqual(culprits, map[string]string{"culprit": "check names: N1 Lib.helper (privatised)"}) {
		t.Fatalf("culprits = %v", culprits)
	}
}

func TestGraphCheckFlagsReturnNoMember(t *testing.T) {
	builder := green("tree-editor")
	ledger := &ledgerFake{}
	runner := testRunner(ledger, builder)
	runner.Checks = []Checker{&graphCheckerFake{name: "statements", lines: []string{
		"flag\tchanged\tLib.size\tLib/Use.lean:9:20\tS1: the statement of Lib.size changed in Lib/Core.lean",
	}}}
	git := runner.Git.(*gitFake)
	git.changes["editor"] = changes("Core.lean")
	git.evidence["editor"] = edited("Lib.size")
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("editor")}})
	if err != nil {
		t.Fatal(err)
	}
	v := checkVerdict(batch, "statements")
	want := []Flag{{Check: "statements", Member: "editor", Declaration: "Lib.size", Kind: blame.Changed, Path: "Lib/Use.lean", Line: 9,
		Reason: "S1: the statement of Lib.size changed in Lib/Core.lean"}}
	if len(batch.Dropped) != 0 || len(ledger.returned) != 0 || v.Verdict != "flag" || !reflect.DeepEqual(v.Flags, want) || len(builder.trees) != 1 {
		t.Fatalf("dropped %v, verdict %+v, builds %d", batch.Dropped, v, len(builder.trees))
	}
	if batch.State != Green || batch.Receipt == nil || !reflect.DeepEqual(batch.Receipt.Checks[0].Flags, want) {
		t.Fatalf("state %s, receipt %+v, want green with the flag kept", batch.State, batch.Receipt)
	}
}

func TestUnattributedFindingMakesTheCheckUnableAndTheBuildDecides(t *testing.T) {
	builder := green("tree-culprit+quiet")
	ledger := &ledgerFake{}
	runner := testRunner(ledger, builder)
	runner.Checks = []Checker{&graphCheckerFake{name: "names", lines: []string{
		"refuse\tunknown\tLib.helper\tLib/Use.lean:4:3\tN1: Lib/Core.lean no longer declares Lib.helper publicly",
		"refuse\tunknown\tLib.gone\tLib/Use.lean:9:3\tN1: Lib/Core.lean no longer declares Lib.gone",
	}}}
	git := runner.Git.(*gitFake)
	git.evidence["culprit"] = privatised("Lib.helper")
	git.evidence["quiet"] = edited("Lib.unrelated")
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base",
		Members: []Member{member("culprit"), member("quiet")}})
	if err != nil {
		t.Fatal(err)
	}
	v := checkVerdict(batch, "names")
	if len(batch.Dropped) != 0 || len(ledger.returned) != 0 || v.Verdict != "unable" || len(v.Unattributed) != 1 ||
		v.Unattributed[0].Declaration != "Lib.gone" || len(v.Refused) != 0 || !strings.Contains(v.Reason, "no evidence") || len(builder.trees) != 1 {
		t.Fatalf("dropped %v, verdict %+v, builds %d", batch.Dropped, v, len(builder.trees))
	}
	if batch.State != Green {
		t.Fatalf("state %s (%s), want green: the build decides", batch.State, batch.Reason)
	}
}

func TestBroadEvidenceMakesTheCheckUnable(t *testing.T) {
	ids := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	builder := green("tree-" + strings.Join(ids, "+"))
	runner := testRunner(&ledgerFake{}, builder)
	runner.Checks = []Checker{&graphCheckerFake{name: "names", lines: []string{
		"flag\tunknown\tLib.helper\tLib/Use.lean:4:3\tN1: Lib/Core.lean no longer declares Lib.helper publicly",
	}}}
	var members []Member
	for _, id := range ids {
		runner.Git.(*gitFake).evidence[id] = privatised("Lib.helper")
		members = append(members, member(id))
	}
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: members})
	if err != nil {
		t.Fatal(err)
	}
	v := checkVerdict(batch, "names")
	if len(batch.Dropped) != 0 || v.Verdict != "unable" || len(v.Flags) != 0 || !strings.Contains(v.Reason, "too many strong matches") || len(builder.trees) != 1 {
		t.Fatalf("dropped %v, verdict %+v, builds %d", batch.Dropped, v, len(builder.trees))
	}
}

func TestCommandCheckerPassesTheBaseAndParsesStdout(t *testing.T) {
	checker := CommandChecker{CheckName: "names", Format: "graph", Command: []string{"sh", "-c",
		`test "$LAND_BASE" = base && test "$LAND_TREE" = tree || exit 9; printf 'refuse\tunknown\tA.b\tA.lean:1:1\tN1: x\n'`}}
	got, err := checker.Check(context.Background(), CheckInput{Tree: "tree", Base: "base"})
	if err != nil || len(got) != 1 || got[0].Declaration != "A.b" || got[0].Verdict != VerdictRefuse {
		t.Fatalf("Check = %+v, %v", got, err)
	}
}

func TestCommandCheckerRefusesStderrOnSuccess(t *testing.T) {
	for _, format := range []string{"graph", "findings"} {
		checker := CommandChecker{CheckName: "names", Format: format, Command: []string{"sh", "-c",
			`printf 'path\treason\n' >&2; exit 0`}}
		if _, err := checker.Check(context.Background(), CheckInput{Tree: "tree", Base: "base"}); err == nil || !strings.Contains(err.Error(), "path") {
			t.Fatalf("%s: Check error = %v, want an error quoting the stderr", format, err)
		}
	}
}

func TestCommandCheckerRefusesAMalformedGraphLine(t *testing.T) {
	checker := CommandChecker{CheckName: "names", Format: "graph", Command: []string{"sh", "-c", `printf 'path\treason\n'`}}
	if _, err := checker.Check(context.Background(), CheckInput{Tree: "tree", Base: "base"}); err == nil {
		t.Fatal("Check accepted a findings line as a graph line")
	}
}

func TestLedgerRefusalComesBeforeTheGraphChecks(t *testing.T) {
	ledger := &ledgerFake{refusals: map[string]string{"w": "undeclared_write"}}
	runner := testRunner(ledger, green("tree-keep"))
	check := &graphCheckerFake{name: "names"}
	runner.Checks = []Checker{check}
	if _, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base",
		Members: []Member{member("w"), member("keep")}}); err != nil {
		t.Fatal(err)
	}
	if len(check.saw) == 0 || check.saw[0].Tree != "tree-keep" {
		t.Fatalf("the graph check saw %+v, want only the candidate without w", check.saw)
	}
}

type unableChecker struct{ name string }

func (u unableChecker) Name() string { return u.name }
func (u unableChecker) Check(context.Context, CheckInput) ([]Finding, error) {
	return nil, &CheckUnableError{Check: u.name, Reason: "the index is of another tree"}
}

func TestUnableGraphCheckLetsTheBuildDecide(t *testing.T) {
	builder := green("tree-one")
	runner := testRunner(&ledgerFake{}, builder)
	second := &graphCheckerFake{name: "statements"}
	runner.Checks = []Checker{unableChecker{name: "names"}, second}
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if v := checkVerdict(batch, "names"); batch.State != Green || v.Verdict != "unable" || v.Reason != "the index is of another tree" || len(builder.trees) != 1 {
		t.Fatalf("state %s, verdict %+v, builds %d", batch.State, v, len(builder.trees))
	}
	if len(second.saw) != 1 || checkVerdict(batch, "statements").Verdict != "pass" {
		t.Fatalf("the next check ran %d times, verdict %+v", len(second.saw), checkVerdict(batch, "statements"))
	}
	if batch.Receipt == nil || len(batch.Receipt.Checks) != 2 || batch.Receipt.Checks[0].Verdict != "unable" {
		t.Fatalf("receipt %+v, want the unable verdict kept", batch.Receipt)
	}
}

func TestBrokenGraphCheckFailsTheBatch(t *testing.T) {
	runner := testRunner(&ledgerFake{}, green("tree-one"))
	runner.Checks = []Checker{CommandChecker{CheckName: "names", Format: "graph", Command: []string{"sh", "-c", "exit 2"}}}
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if batch.State != Failed || batch.Reason != "check-unable" {
		t.Fatalf("state %s, reason %q, want failed and check-unable", batch.State, batch.Reason)
	}
}

func TestCommandCheckerMapsExitThreeToUnable(t *testing.T) {
	checker := CommandChecker{CheckName: "names", Format: "graph", Command: []string{"sh", "-c", "echo 'index of another tree' >&2; exit 3"}}
	_, err := checker.Check(context.Background(), CheckInput{Tree: "tree", Base: "base"})
	var unable *CheckUnableError
	if !errors.As(err, &unable) || unable.Reason != "index of another tree" {
		t.Fatalf("Check error = %v, want *CheckUnableError with the stderr line", err)
	}
}

// A findings-format check that exits 3 is not unable: only a graph check is.
func TestFindingsCheckExitThreeFailsTheBatch(t *testing.T) {
	runner := testRunner(&ledgerFake{}, green("tree-one"))
	runner.Checks = []Checker{CommandChecker{CheckName: "files", Command: []string{"sh", "-c", "exit 3"}}}
	batch, err := runner.Run(context.Background(), Input{ID: "b", Repository: "repo", Base: "base", Members: []Member{member("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if batch.State != Failed || batch.Reason != "check-unable" {
		t.Fatalf("state %s, reason %q, want failed and check-unable", batch.State, batch.Reason)
	}
}

// I1's field line names the field qualified. The member that removed the
// field, read through the real Git adapter, is refused; a member that edited
// another file is not.
func TestGraphFieldFindingRefusesTheMemberThatRemovedTheField(t *testing.T) {
	core := "namespace Lib.Core\nstructure Box where\n  val : Nat\n  size : Nat\nend Lib.Core\n"
	repo, root := commandRepository(t, false)
	writeSource(t, repo, "Core.lean", core)
	writeSource(t, repo, "Other.lean", "def other := 0\n")
	commandReviewGit(t, repo, "add", "-A")
	commandReviewGit(t, repo, "commit", "-qm", "base")
	base := commandReviewGit(t, repo, "rev-parse", "HEAD")
	branch := func(name, path, source string) Member {
		commandReviewGit(t, repo, "checkout", "-q", "-b", name, base)
		writeSource(t, repo, path, source)
		commandReviewGit(t, repo, "commit", "-qam", name)
		return Member{Migration: name, Generation: 1, Head: commandReviewGit(t, repo, "rev-parse", "HEAD")}
	}
	culprit := branch("culprit", "Core.lean", strings.Replace(core, "  val : Nat\n", "", 1))
	bystander := branch("bystander", "Other.lean", "def other := 1\n")
	_ = root
	runner := testRunner(&ledgerFake{}, nil)
	runner.Git = RepositoryGit{}
	runner.Checks = []Checker{&graphCheckerFake{name: "names", lines: []string{
		"refuse\tfield\tLib.Core.Box.val\tLib/Use.lean:8:42\tN1: structure Lib.Core.Box in Lib/Core.lean no longer has field val",
	}}}
	candidate, err := runner.Git.Candidate(context.Background(), repo, base, []Member{culprit, bystander})
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{Repository: repo, Base: base, Candidate: candidate, ActiveMembers: []Member{culprit, bystander}}
	verdicts, culprits, err := runner.evaluateChecks(context.Background(), &batch, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Verdict != "fail" || len(verdicts[0].Refused) != 1 || verdicts[0].Refused[0].Member != "culprit" {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if _, found := culprits["bystander"]; found || len(culprits) != 1 {
		t.Fatalf("culprits = %v, want culprit only", culprits)
	}
}
