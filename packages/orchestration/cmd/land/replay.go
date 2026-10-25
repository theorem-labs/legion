package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
	"github.com/sjawhar/legion/orchestration/landing/gitx"
	"github.com/sjawhar/legion/orchestration/replay"
)

// RoundSummary is what land replay found in one recorded build round.
type RoundSummary struct {
	Round             int                `json:"round"`
	Members           int                `json:"members"`      // members in this build round's candidate
	CheckSeconds      map[string]float64 `json:"checkSeconds"` // check name -> wall seconds
	Refused           []SummaryRefusal   `json:"refused"`
	Flagged           []SummaryFlag      `json:"flagged"`
	BuildNamed        []SummaryBuild     `json:"buildNamed"`        // the build blame's attributions on the recorded log
	RecordedNameKicks []replay.Kick      `json:"recordedNameKicks"` // the round's recorded ident, clash and unprune kicks
	FalseRefusals     []SummaryRefusal   `json:"falseRefusals"`     // refusals whose confirmation build was green
	Missed            []SummaryMiss      `json:"missed"`            // build-named members that no check refused or flagged for the name
	BlameWrong        []SummaryBuild     `json:"blameWrong"`        // build-named members whose module is green with the member alone
	Unable            []SummaryUnable    `json:"unable"`            // checks that were unable in this build round
}

// SummaryRefusal is one check refusal; Outcome is the row's.
type SummaryRefusal struct {
	Member      string `json:"member"`
	Declaration string `json:"declaration"`
	Rule        string `json:"rule"`
	Outcome     string `json:"outcome"`
}

// SummaryFlag is one check flag.
type SummaryFlag struct {
	Member      string `json:"member"`
	Declaration string `json:"declaration"`
	Rule        string `json:"rule"`
}

// SummaryBuild is one build blame attribution.
type SummaryBuild struct {
	Member   string     `json:"member"`
	Module   string     `json:"module"`
	Name     string     `json:"name"`
	Kind     blame.Kind `json:"kind"`
	Strength int        `json:"strength"`
}

// SummaryMiss is a name error the checks missed. Confirmation is
// confirmed-red or unconfirmed-infra, and empty without --confirm-with.
type SummaryMiss struct {
	Member       string `json:"member"`
	Name         string `json:"name"`
	Cause        string `json:"cause"`
	Confirmation string `json:"confirmation"`
}

// SummaryUnable is a check that was unable in a build round.
type SummaryUnable struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// The outcomes of a replay row.
const (
	explainedByLog   = "explained-by-log"
	confirmedRed     = "confirmed-red"
	confirmedGreen   = "confirmed-green"
	unconfirmedInfra = "unconfirmed-infra"
)

// errReplayUnconfirmed makes land exit 1 after land replay printed its rows and
// wrote its summary: a confirmation build failed on infrastructure twice.
var errReplayUnconfirmed = errors.New("a confirmation build failed on infrastructure twice")

// runReplayCommand replays a recorded batch, build round by build round: the
// configured checks on the round's candidate, the build blame on the round's
// recorded log, and the recorded program's kicks. It prints one row per
// finding,
//
//	round<TAB>member<TAB>source<TAB>rule-or-module<TAB>declaration<TAB>kind<TAB>evidence<TAB>outcome
//
// and with --summary writes one RoundSummary per build round.
func runReplayCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", "", "replay manifest")
	configPath := flags.String("config", "", "land configuration")
	only := flags.Int("round", 0, "replay only this build round")
	confirm := flags.String("confirm-with", "", "confirmation build command")
	summaryPath := flags.String("summary", "", "write the round summaries here")
	usage := usageError{fmt.Errorf("usage: land replay --manifest FILE --config FILE [--round R] [--confirm-with CMD] [--summary FILE]")}
	if err := flags.Parse(args); err != nil {
		return usageError{err}
	}
	if *manifestPath == "" || *configPath == "" || flags.NArg() != 0 {
		return usage
	}
	manifest, err := replay.Load(*manifestPath)
	if err != nil {
		return err
	}
	if *only < 0 || *only > len(manifest.BuildRounds) {
		return usageError{fmt.Errorf("--round %d: the manifest has build rounds 1 to %d", *only, len(manifest.BuildRounds))}
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.Repository.Path == "" {
		cfg.Repository.Path = manifest.Repository
	}
	r := &replayer{
		manifest:   manifest,
		logDir:     filepath.Dir(*manifestPath),
		roots:      cfg.ModuleRoots,
		runner:     landing.Runner{Git: landing.RepositoryGit{Path: manifest.Repository}, Checks: cfg.checkers()},
		blames:     map[int]*roundBlame{},
		unresolved: map[string]bool{},
		files:      map[string][]string{},
		added:      map[string]map[string]bool{},
		output:     output,
	}
	if *confirm != "" {
		r.builder = &landing.CommandBuilder{RepositoryPath: manifest.Repository, Command: []string{"sh", "-c", *confirm}}
	}
	for _, unresolved := range manifest.Unresolved {
		r.unresolved[unresolved.ID] = true
	}
	summaries := []RoundSummary{}
	for index := range manifest.BuildRounds {
		if *only != 0 && index != *only-1 {
			continue
		}
		summary, err := r.round(ctx, index)
		if err != nil {
			return err
		}
		summaries = append(summaries, summary)
	}
	if *summaryPath != "" {
		body, err := json.MarshalIndent(summaries, "", "  ")
		if err != nil {
			return fmt.Errorf("encode summary: %w", err)
		}
		if err := os.WriteFile(*summaryPath, append(body, '\n'), 0o644); err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
	}
	if r.infra {
		return errReplayUnconfirmed
	}
	return nil
}

type replayer struct {
	manifest   replay.Manifest
	logDir     string
	roots      []string
	runner     landing.Runner
	builder    *landing.CommandBuilder
	blames     map[int]*roundBlame
	unresolved map[string]bool
	files      map[string][]string        // tree:path -> its lines
	added      map[string]map[string]bool // member\x00path -> the trimmed text of its added lines
	infra      bool
	output     io.Writer
}

// roundBlame is the build blame of one recorded build round.
type roundBlame struct {
	tree         string
	members      map[string]landing.Member // the candidate's members
	report       blame.Report
	lines        map[string]blame.TreeLines
	attributions []blame.Attribution
}

// roundMembers is the manifest's members less the build round's excluded ones.
func (r *replayer) roundMembers(index int) []landing.Member {
	excluded := make(map[string]bool, len(r.manifest.BuildRounds[index].Excluded))
	for _, id := range r.manifest.BuildRounds[index].Excluded {
		excluded[id] = true
	}
	var members []landing.Member
	for _, member := range r.manifest.Members {
		if !excluded[member.ID] {
			members = append(members, landing.Member{Migration: member.ID, Generation: 1, Head: member.Head})
		}
	}
	return members
}

// blameFor runs, once, land blame's attribution on a build round's recorded
// log with the round's candidate members.
func (r *replayer) blameFor(ctx context.Context, index int) (*roundBlame, error) {
	if found, ok := r.blames[index]; ok {
		return found, nil
	}
	members := r.roundMembers(index)
	inputs := make([]gitx.Member, len(members))
	for i, member := range members {
		inputs[i] = gitx.Member{ID: member.Migration, Head: member.Head}
	}
	candidate, err := gitx.Candidate(ctx, r.manifest.Repository, r.manifest.Base, inputs)
	if err != nil {
		return nil, fmt.Errorf("build round %d candidate: %w", index+1, err)
	}
	dropped := make(map[string]bool, len(candidate.Dropped))
	for _, drop := range candidate.Dropped {
		dropped[drop.ID] = true
	}
	result := &roundBlame{tree: candidate.Tree, members: map[string]landing.Member{}}
	var blameMembers []blame.Member
	for _, member := range members {
		if !dropped[member.Migration] {
			result.members[member.Migration] = member
			blameMembers = append(blameMembers, blame.Member{ID: member.Migration, Head: member.Head})
		}
	}
	log, err := os.ReadFile(filepath.Join(r.logDir, r.manifest.BuildRounds[index].Log))
	if err != nil {
		return nil, fmt.Errorf("read build round %d log: %w", index+1, err)
	}
	result.report = blame.Parse(string(log), r.roots)
	lines, ambiguous, err := collectTreeLines(ctx, r.manifest.Repository, blame.Tree(r.manifest.Base), blame.Tree(candidate.Tree), result.report)
	if err != nil {
		return nil, err
	}
	evidence, err := collectEvidence(ctx, r.manifest.Repository, r.manifest.Base, blameMembers, ambiguous)
	if err != nil {
		return nil, err
	}
	result.lines = lines
	result.attributions = blame.IdentifyDetailed(result.report, evidence, lines)
	r.blames[index] = result
	return result, nil
}

// round replays one build round.
func (r *replayer) round(ctx context.Context, index int) (RoundSummary, error) {
	number := r.manifest.BuildRounds[index].Round
	report, err := r.runner.CheckCandidate(ctx, landing.Input{ID: r.manifest.Name, Repository: r.manifest.Repository, Base: r.manifest.Base, Members: r.roundMembers(index)})
	if err != nil {
		return RoundSummary{}, fmt.Errorf("build round %d checks: %w", number, err)
	}
	summary := RoundSummary{Round: number, Members: len(report.Members), CheckSeconds: map[string]float64{},
		Refused: []SummaryRefusal{}, Flagged: []SummaryFlag{}, BuildNamed: []SummaryBuild{}, RecordedNameKicks: []replay.Kick{},
		FalseRefusals: []SummaryRefusal{}, Missed: []SummaryMiss{}, BlameWrong: []SummaryBuild{}, Unable: []SummaryUnable{}}
	// covered holds, per member, the declarations a check refused or flagged.
	covered := map[string][]string{}
	for _, verdict := range report.Checks {
		summary.CheckSeconds[verdict.Name] = verdict.Seconds
		if verdict.Verdict == "unable" {
			summary.Unable = append(summary.Unable, SummaryUnable{Check: verdict.Name, Reason: verdict.Reason})
		}
		for _, finding := range verdict.Unattributed {
			if err := r.row(number, "", "check-unattributed", rule(finding.Reason), finding.Declaration, finding.Kind, finding.Note, ""); err != nil {
				return RoundSummary{}, err
			}
		}
		for _, refusal := range verdict.Refused {
			outcome, err := r.refusalOutcome(ctx, index, verdict, refusal)
			if err != nil {
				return RoundSummary{}, err
			}
			if err := r.row(number, refusal.Member, "check-refuse", rule(refusal.Reason), refusal.Declaration, refusal.Kind, refusal.Evidence, outcome); err != nil {
				return RoundSummary{}, err
			}
			entry := SummaryRefusal{Member: refusal.Member, Declaration: refusal.Declaration, Rule: rule(refusal.Reason), Outcome: outcome}
			summary.Refused = append(summary.Refused, entry)
			if outcome == confirmedGreen {
				summary.FalseRefusals = append(summary.FalseRefusals, entry)
			}
			covered[refusal.Member] = append(covered[refusal.Member], refusal.Declaration)
		}
		for _, flag := range verdict.Flags {
			if err := r.row(number, flag.Member, "check-flag", rule(flag.Reason), flag.Declaration, flag.Kind, "", ""); err != nil {
				return RoundSummary{}, err
			}
			summary.Flagged = append(summary.Flagged, SummaryFlag{Member: flag.Member, Declaration: flag.Declaration, Rule: rule(flag.Reason)})
			covered[flag.Member] = append(covered[flag.Member], flag.Declaration)
		}
	}
	built, err := r.blameFor(ctx, index)
	if err != nil {
		return RoundSummary{}, err
	}
	missed := map[string]bool{}
	for _, attribution := range built.attributions {
		if err := r.row(number, attribution.Member, "build", attribution.Module, attribution.Name, attribution.Kind, attribution.Evidence, ""); err != nil {
			return RoundSummary{}, err
		}
		named := SummaryBuild{Member: attribution.Member, Module: attribution.Module, Name: attribution.Name, Kind: attribution.Kind, Strength: attribution.Strength}
		summary.BuildNamed = append(summary.BuildNamed, named)
		if coversName(covered[attribution.Member], attribution.Name) {
			continue
		}
		miss := SummaryMiss{Member: attribution.Member, Name: attribution.Name, Cause: r.cause(ctx, built, attribution)}
		if r.builder != nil {
			outcome, err := r.confirmation(ctx, attribution.Module, []string{attribution.Member})
			if err != nil {
				return RoundSummary{}, err
			}
			if outcome == confirmedGreen {
				summary.BlameWrong = append(summary.BlameWrong, named)
				continue
			}
			miss.Confirmation = outcome
		}
		if key := miss.Member + "\x00" + miss.Name; !missed[key] {
			missed[key] = true
			summary.Missed = append(summary.Missed, miss)
		}
	}
	for _, kick := range r.manifest.BuildRounds[index].RecordedKicks {
		reasons, name, nameKick := parseKick(kick.Reason)
		if err := r.row(number, kick.Member, "recorded", reasons, name, "", "", ""); err != nil {
			return RoundSummary{}, err
		}
		if !nameKick {
			continue
		}
		summary.RecordedNameKicks = append(summary.RecordedNameKicks, kick)
		if r.unresolved[kick.Member] {
			// The member's head is not in the manifest, so no check and no
			// build blame can name it.
			summary.Missed = append(summary.Missed, SummaryMiss{Member: kick.Member, Name: name, Cause: "unresolved member"})
		}
	}
	return summary, nil
}

// parseKick reads a recorded kick's reason. The recorded program joins one
// kick's reasons with "; ", and a reason may carry a name after a colon
// ("ident:X; red"). It returns the reasons without their names, joined the
// same way, the first name, and whether one reason is a name error (ident,
// clash or unprune).
func parseKick(reason string) (string, string, bool) {
	var kinds []string
	name, nameKick := "", false
	for _, part := range strings.Split(reason, ";") {
		kind, value, _ := strings.Cut(strings.TrimSpace(part), ":")
		kinds = append(kinds, kind)
		if name == "" {
			name = value
		}
		nameKick = nameKick || kind == "ident" || kind == "clash" || kind == "unprune"
	}
	return strings.Join(kinds, "; "), name, nameKick
}

// refusalOutcome is explained-by-log when the recorded log of this build round
// or a later one, while the member is still in the candidate, has an error the
// build blame attributes to the member for the refused declaration. Otherwise,
// with --confirm-with, it is the confirmation build of the finding's module on
// the base with the member, and for an added finding with the other members
// the check refused for the same declaration.
func (r *replayer) refusalOutcome(ctx context.Context, index int, verdict landing.CheckVerdict, refusal landing.Refusal) (string, error) {
	if refusal.Declaration != "" {
		for later := index; later < len(r.manifest.BuildRounds); later++ {
			built, err := r.blameFor(ctx, later)
			if err != nil {
				return "", err
			}
			if _, present := built.members[refusal.Member]; !present {
				break
			}
			for _, attribution := range built.attributions {
				if attribution.Member == refusal.Member && namesDeclaration(attribution.Name, refusal.Declaration) {
					return explainedByLog, nil
				}
			}
		}
	}
	if r.builder == nil {
		return "", nil
	}
	members := []string{refusal.Member}
	if refusal.Kind == blame.Added {
		for _, other := range verdict.Refused {
			if other.Declaration == refusal.Declaration && !contains(members, other.Member) {
				members = append(members, other.Member)
			}
		}
	}
	return r.confirmation(ctx, moduleOf(refusal.Path), members)
}

// confirmation builds module on the base with members through the build
// adapter. An infrastructure failure is retried once; a second one is
// unconfirmed-infra and makes land replay fail after it reported.
func (r *replayer) confirmation(ctx context.Context, module string, members []string) (string, error) {
	inputs := make([]gitx.Member, 0, len(members))
	for _, id := range members {
		for _, member := range r.manifest.Members {
			if member.ID == id {
				inputs = append(inputs, gitx.Member{ID: member.ID, Head: member.Head})
			}
		}
	}
	candidate, err := gitx.Candidate(ctx, r.manifest.Repository, r.manifest.Base, inputs)
	if err != nil {
		return "", fmt.Errorf("confirmation candidate of %v: %w", members, err)
	}
	for range 2 {
		result, err := r.builder.Build(ctx, landing.Tree(candidate.Tree), []string{module})
		if err != nil || result.InfraClass != "" {
			continue
		}
		if result.Green || result.Absent {
			return confirmedGreen, nil
		}
		return confirmedRed, nil
	}
	r.infra = true
	return unconfirmedInfra, nil
}

// cause classifies a build-named name error that no check refused or flagged.
func (r *replayer) cause(ctx context.Context, built *roundBlame, attribution blame.Attribution) string {
	switch {
	case r.useAdded(ctx, built, attribution):
		return "use added in the same batch"
	case len(built.lines[attribution.Name].Base) == 0 && len(built.lines[attribution.Name].Candidate) == 0:
		return "generated name"
	case attribution.Kind == blame.Weak || attribution.Kind == blame.Changed && attribution.Strength < 3:
		return "not a name error"
	}
	return "defect"
}

// useAdded reports whether a use the error names is on a line that a member
// of the build round added.
func (r *replayer) useAdded(ctx context.Context, built *roundBlame, attribution blame.Attribution) bool {
	for _, block := range built.report.Blocks {
		if block.Module != attribution.Module || block.Line < 1 || !blockNames(block, attribution.Name) {
			continue
		}
		lines := r.fileLines(ctx, built.tree, block.Path)
		if block.Line > len(lines) {
			continue
		}
		use := strings.TrimSpace(lines[block.Line-1])
		if use == "" {
			continue
		}
		for _, member := range built.members {
			if r.addedLines(ctx, member, block.Path)[use] {
				return true
			}
		}
	}
	return false
}

func blockNames(block blame.Block, name string) bool {
	for _, candidate := range blame.Names(block.Message) {
		if candidate.Value == name {
			return true
		}
	}
	return false
}

// fileLines reads, once, the lines of path in tree; a missing file has none.
func (r *replayer) fileLines(ctx context.Context, tree, path string) []string {
	key := tree + ":" + path
	if lines, found := r.files[key]; found {
		return lines
	}
	output, err := exec.CommandContext(ctx, "git", "-C", r.manifest.Repository, "cat-file", "-p", key).Output()
	var lines []string
	if err == nil {
		lines = strings.Split(string(output), "\n")
	}
	r.files[key] = lines
	return lines
}

// addedLines reads, once, the trimmed text of the lines a member's diff from
// its merge base with the base adds to path.
func (r *replayer) addedLines(ctx context.Context, member landing.Member, path string) map[string]bool {
	key := member.Migration + "\x00" + path
	if added, found := r.added[key]; found {
		return added
	}
	added := map[string]bool{}
	output, err := exec.CommandContext(ctx, "git", "-C", r.manifest.Repository, "diff", "-U0", "--no-color", "--no-ext-diff",
		r.manifest.Base+"..."+member.Head, "--", path).Output()
	if err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				if text := strings.TrimSpace(line[1:]); text != "" {
					added[text] = true
				}
			}
		}
	}
	r.added[key] = added
	return added
}

func (r *replayer) row(round int, member, source, ruleOrModule, declaration string, kind blame.Kind, evidence, outcome string) error {
	if _, err := fmt.Fprintf(r.output, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", round, member, source, ruleOrModule, declaration, kind, evidence, outcome); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// rule is the rule of a graph finding's reason, "<rule>: <message>".
func rule(reason string) string {
	prefix, _, _ := strings.Cut(reason, ":")
	return prefix
}

// namesDeclaration reports whether a diagnostic name can refer to a
// declaration's fully qualified name.
func namesDeclaration(name, declaration string) bool {
	return declaration != "" && (blame.Refers(name, blame.Declaration{Name: declaration}) || blame.Refers(declaration, blame.Declaration{Name: name}))
}

func coversName(declarations []string, name string) bool {
	for _, declaration := range declarations {
		if namesDeclaration(name, declaration) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// moduleOf is the Lean module of a source path.
func moduleOf(path string) string {
	return strings.ReplaceAll(strings.TrimSuffix(path, ".lean"), "/", ".")
}
