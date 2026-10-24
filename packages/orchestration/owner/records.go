package owner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SpecFront is the front matter of an owner's current spec. Retired lists the ids of every claim that an earlier
// revision removed; the command keeps it, and no later claim uses one of them again.
type SpecFront struct {
	Owner       string   `json:"owner" yaml:"owner"`
	Revision    int      `json:"revision" yaml:"revision"`
	Epoch       int      `json:"epoch" yaml:"epoch"`
	Complete    bool     `json:"complete" yaml:"complete"`
	Answers     []string `json:"answers" yaml:"answers"`
	Resolves    []string `json:"resolves" yaml:"resolves"`
	Retired     []string `json:"retired" yaml:"retired"`
	AuthorModel string   `json:"author_model" yaml:"author_model"`
}

// Claim is one numbered claim in a spec or need claim tree.
type Claim struct {
	ID    string
	Depth int
	Text  string
	Uses  []string
	By    []string
}

// LintError is one claim-tree violation. Line is one-based within the claim tree.
type LintError struct {
	Line    int
	Message string
}

func (e LintError) Error() string {
	if e.Line <= 0 {
		return e.Message
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Message)
}

var claimLine = regexp.MustCompile(`^( *)([1-9][0-9]*)\. \[(C[1-9][0-9]*)\] (.+)$`)
var claimID = regexp.MustCompile(`^C[1-9][0-9]*$`)

// ParseSpec strictly parses a spec's YAML front matter and its numbered claim tree (ParseClaims).
func ParseSpec(data []byte) (SpecFront, []Claim, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return SpecFront{}, nil, errors.New("spec must begin with YAML front matter")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return SpecFront{}, nil, errors.New("spec YAML front matter is not closed")
	}

	var front SpecFront
	if err := decodeStrictYAML([]byte(strings.Join(lines[1:end], "\n")), &front); err != nil {
		return SpecFront{}, nil, fmt.Errorf("decode spec front matter: %w", err)
	}
	if err := front.validate(); err != nil {
		return SpecFront{}, nil, err
	}
	claims, err := ParseClaims([]byte(strings.Join(lines[end+1:], "\n")))
	if err != nil {
		return SpecFront{}, nil, err
	}
	return front, claims, nil
}

// ParseClaims parses a numbered claim tree, the body of a spec or a need after its front matter: CRLF line ends are
// read as LF, blank lines are skipped, an error names its line within the tree, and a tree with no claim is refused.
func ParseClaims(body []byte) ([]Claim, error) {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	claims := make([]Claim, 0, len(lines))
	for lineNumber, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		claim, err := parseClaim(line)
		if err != nil {
			return nil, fmt.Errorf("claim line %d: %w", lineNumber+1, err)
		}
		claims = append(claims, claim)
	}
	if len(claims) == 0 {
		return nil, errors.New("the claim tree has no claims")
	}
	return claims, nil
}

func (f SpecFront) validate() error {
	if strings.TrimSpace(f.Owner) == "" {
		return errors.New("spec front matter owner is required")
	}
	if f.Revision <= 0 {
		return errors.New("spec front matter revision must be positive")
	}
	if f.Epoch <= 0 {
		return errors.New("spec front matter epoch must be positive")
	}
	if strings.TrimSpace(f.AuthorModel) == "" {
		return errors.New("spec front matter author_model is required")
	}
	if err := validRetired(f.Retired); err != nil {
		return fmt.Errorf("spec front matter %w", err)
	}
	return nil
}

func parseClaim(line string) (Claim, error) {
	matches := claimLine.FindStringSubmatch(line)
	if matches == nil {
		return Claim{}, errors.New("must be an indented numbered [C<n>] claim")
	}
	if len(matches[1])%4 != 0 {
		return Claim{}, errors.New("claim indentation must be a multiple of four spaces")
	}

	content := matches[4]
	byAt := strings.LastIndex(content, " by: ")
	if byAt < 0 {
		return Claim{}, errors.New("claim must include by:")
	}
	by, err := splitClaimList(content[byAt+len(" by: "):])
	if err != nil {
		return Claim{}, fmt.Errorf("by: %w", err)
	}
	content = content[:byAt]

	text := content
	var uses []string
	if usesAt := strings.LastIndex(content, " uses: "); usesAt >= 0 {
		text = content[:usesAt]
		uses, err = splitClaimList(content[usesAt+len(" uses: "):])
		if err != nil {
			return Claim{}, fmt.Errorf("uses: %w", err)
		}
	}
	if strings.TrimSpace(text) == "" {
		return Claim{}, errors.New("claim text is required")
	}
	return Claim{
		ID:    matches[3],
		Depth: len(matches[1])/4 + 1,
		Text:  strings.TrimSpace(text),
		Uses:  uses,
		By:    by,
	}, nil
}

func splitClaimList(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("contains an empty entry")
		}
		values = append(values, part)
	}
	return values, nil
}

// LintClaims validates a claim tree: unique ids, no id of retired (the ids earlier revisions removed) used again,
// every local use resolved and every remote use well formed. A claim revised under its id keeps that id.
func LintClaims(cur []Claim, retired []string) []LintError {
	var errs []LintError
	gone := make(map[string]bool, len(retired))
	for _, id := range retired {
		gone[id] = true
	}
	current := make(map[string]Claim, len(cur))
	for index, claim := range cur {
		line := index + 1
		switch {
		case !claimID.MatchString(claim.ID):
			errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("invalid claim id %q", claim.ID)})
			continue
		case gone[claim.ID]:
			errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("claim id %s was retired by an earlier revision and is never used again", claim.ID)})
		}
		if _, exists := current[claim.ID]; exists {
			errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("duplicate claim id %s", claim.ID)})
			continue
		}
		current[claim.ID] = claim
	}

	for index, claim := range cur {
		line := index + 1
		for _, use := range claim.Uses {
			if strings.Contains(use, "#") {
				if !validRemoteUse(use) {
					errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("malformed remote use %q", use)})
				}
				continue
			}
			if !claimID.MatchString(use) {
				errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("malformed local use %q", use)})
				continue
			}
			if _, exists := current[use]; !exists {
				errs = append(errs, LintError{Line: line, Message: fmt.Sprintf("local use %s does not resolve", use)})
			}
		}
	}
	return errs
}

// RetiredIDs is the retired list of a new revision: the previous revision's, plus each id of its claims that the new
// claim tree no longer holds, in claim-number order.
func RetiredIDs(retired []string, prev, cur []Claim) []string {
	ids := map[string]bool{}
	for _, id := range retired {
		ids[id] = true
	}
	kept := make(map[string]bool, len(cur))
	for _, claim := range cur {
		kept[claim.ID] = true
	}
	for _, claim := range prev {
		if !kept[claim.ID] {
			ids[claim.ID] = true
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b string) int { return claimNumber(a) - claimNumber(b) })
	return out
}

// claimNumber is n of a claim id C<n>, which claimID has checked.
func claimNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "C"))
	return n
}

func validRetired(retired []string) error {
	for _, id := range retired {
		if !claimID.MatchString(id) {
			return fmt.Errorf("retired claim %q is invalid", id)
		}
	}
	return nil
}

func validRemoteUse(use string) bool {
	parts := strings.Split(use, "#")
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" &&
		!strings.ContainsAny(parts[0], " \t\r\n") && claimID.MatchString(parts[1])
}

// NeedFront is the front matter of a need record (P3): Source and Target are the edge's endpoints. Origin is where
// the requirement came from for measure 6, and a need always comes from a neighbor. Resolves lists the
// requirements the revision closes (replies and review findings); Retired is as in SpecFront.
type NeedFront struct {
	Edge        string   `json:"edge" yaml:"edge"`
	Source      string   `json:"source" yaml:"source"`
	Target      string   `json:"target" yaml:"target"`
	Origin      string   `json:"origin" yaml:"origin"`
	Revision    int      `json:"revision" yaml:"revision"`
	Epoch       int      `json:"epoch" yaml:"epoch"`
	Cites       []string `json:"cites" yaml:"cites"`
	Resolves    []string `json:"resolves" yaml:"resolves"`
	Retired     []string `json:"retired" yaml:"retired"`
	AuthorModel string   `json:"author_model" yaml:"author_model"`
}

// The origins of a requirement (measure 6), each fixed by the kind of record that holds it.
const (
	OriginNeighbor = "neighbor" // a need, or a reply
	OriginReview   = "review"   // a finding of the review of the node's own spec
)

// Reply is an acceptance or a reasoned response to an offer.
type Reply struct {
	Kind         string            `json:"kind" yaml:"kind"`
	Edge         string            `json:"edge" yaml:"edge"`
	Reason       string            `json:"reason,omitempty" yaml:"reason,omitempty"`
	PremiseOwner string            `json:"premise_owner,omitempty" yaml:"premise_owner,omitempty"`
	Answers      map[string]string `json:"answers" yaml:"answers"`
	Epoch        int               `json:"epoch" yaml:"epoch"`
	At           time.Time         `json:"at" yaml:"at"`
	Text         string            `json:"text" yaml:"text"`
}

// EdgeNeed identifies the committed need that an edge record reads.
type EdgeNeed struct {
	Path string `json:"path" yaml:"path"`
	Hash string `json:"hash" yaml:"hash"`
}

// EdgeOffer identifies the target spec revision that an edge record reads.
type EdgeOffer struct {
	SpecHash string `json:"spec_hash" yaml:"spec_hash"`
}

// EdgeLean identifies the English-to-Lean translation and proof of an edge.
type EdgeLean struct {
	NeedDecl  string   `json:"need_decl" yaml:"need_decl"`
	OfferDecl string   `json:"offer_decl" yaml:"offer_decl"`
	Module    string   `json:"module" yaml:"module"`
	Theorem   string   `json:"theorem" yaml:"theorem"`
	Cites     []string `json:"cites" yaml:"cites"`
}

// EdgeRecord is the current state of an edge between two owner nodes.
type EdgeRecord struct {
	Edge               string    `json:"edge" yaml:"edge"`
	Source             string    `json:"source" yaml:"source"`
	Target             string    `json:"target" yaml:"target"`
	Status             string    `json:"status" yaml:"status"`
	Need               EdgeNeed  `json:"need" yaml:"need"`
	Offer              EdgeOffer `json:"offer" yaml:"offer"`
	Agreement          string    `json:"agreement" yaml:"agreement"`
	Lean               EdgeLean  `json:"lean" yaml:"lean"`
	TranslationReviews []string  `json:"translation_reviews" yaml:"translation_reviews"`
	BuiltAt            time.Time `json:"built_at" yaml:"built_at"`
}

// Finding is one result of a review pass. Origin is always review (measure 6), and Node is the node whose own spec
// the pass reviewed.
type Finding struct {
	ID         string `json:"id" yaml:"id"`
	Severity   string `json:"severity" yaml:"severity"`
	Text       string `json:"text" yaml:"text"`
	ResolvedBy string `json:"resolved_by,omitempty" yaml:"resolved_by,omitempty"`
	Origin     string `json:"origin" yaml:"origin"`
	Node       string `json:"node" yaml:"node"`
}

// Review records one model pass over a spec, need, Lean statement, or migration.
type Review struct {
	Subject  string    `json:"subject" yaml:"subject"`
	Revision int       `json:"revision" yaml:"revision"`
	Pass     int       `json:"pass" yaml:"pass"`
	Model    string    `json:"model" yaml:"model"`
	Findings []Finding `json:"findings" yaml:"findings"`
}

// ReviewPass is one review pass preserved in an upstream review record.
type ReviewPass struct {
	Pass     int       `json:"pass" yaml:"pass"`
	Model    string    `json:"model" yaml:"model"`
	Findings []Finding `json:"findings" yaml:"findings"`
}

// UpstreamReview records a contract owner's review of an upstream statement change.
type UpstreamReview struct {
	Contract string       `json:"contract" yaml:"contract"`
	Reviewer string       `json:"reviewer" yaml:"reviewer"`
	Upstream string       `json:"upstream" yaml:"upstream"`
	Head     string       `json:"head" yaml:"head"`
	FromHash string       `json:"from_hash" yaml:"from_hash"`
	ToHash   string       `json:"to_hash" yaml:"to_hash"`
	Verdict  string       `json:"verdict" yaml:"verdict"`
	Reason   string       `json:"reason,omitempty" yaml:"reason,omitempty"`
	Passes   []ReviewPass `json:"passes" yaml:"passes"`
	At       time.Time    `json:"at" yaml:"at"`
}

// NodeMeta is the owner metadata stored in node.yaml.
type NodeMeta struct {
	ID           string   `json:"id" yaml:"id"`
	Key          string   `json:"key" yaml:"key"`
	Kind         string   `json:"kind" yaml:"kind"`
	Role         string   `json:"role" yaml:"role"`
	LeanDir      string   `json:"lean_dir" yaml:"lean_dir"`
	CFile        string   `json:"c_file" yaml:"c_file"`
	Active       bool     `json:"active" yaml:"active"`
	Parent       string   `json:"parent,omitempty" yaml:"parent,omitempty"`
	Children     []string `json:"children" yaml:"children"`
	ContractDecl string   `json:"contract_decl,omitempty" yaml:"contract_decl,omitempty"`
}

// Escalation records evidence that must reach the escalations queue.
type Escalation struct {
	Subject        string `json:"subject" yaml:"subject"`
	Record         string `json:"record" yaml:"record"`
	Reason         string `json:"reason" yaml:"reason"`
	Counterexample string `json:"counterexample,omitempty" yaml:"counterexample,omitempty"`
	BugsRow        string `json:"bugs_row,omitempty" yaml:"bugs_row,omitempty"`
}

// Produced records an interface producer that an owner added to its import closure.
type Produced struct {
	Interface   string `json:"interface" yaml:"interface"`
	Module      string `json:"module" yaml:"module"`
	Declaration string `json:"declaration" yaml:"declaration"`
	Migration   string `json:"migration" yaml:"migration"`
}

var replyReasons = map[string]struct{}{
	"proved-incompatible":     {},
	"missing-premise":         {},
	"proof-incomplete":        {},
	"superseded":              {},
	"translation-error":       {},
	"program-defect":          {},
	"false-shared-assumption": {},
}

var edgeStatuses = map[string]struct{}{
	"open": {}, "accepted": {}, "agreed": {}, "lean_stated": {}, "translation_ok": {}, "proved": {}, "outside_part": {},
}

func (n NeedFront) validate() error {
	source, target, err := ParseEdgeID(n.Edge)
	if err != nil {
		return fmt.Errorf("need edge: %w", err)
	}
	if n.Source != source {
		return fmt.Errorf("need source %q must be the edge's source %q", n.Source, source)
	}
	if n.Target != target {
		return fmt.Errorf("need target %q must be the edge's target %q", n.Target, target)
	}
	if n.Origin != OriginNeighbor {
		return fmt.Errorf("need origin %q must be %s: a need comes from a neighbor", n.Origin, OriginNeighbor)
	}
	if n.Revision <= 0 || n.Epoch <= 0 {
		return errors.New("need revision and epoch must be positive")
	}
	if strings.TrimSpace(n.AuthorModel) == "" {
		return errors.New("need author_model is required")
	}
	for _, requirement := range n.Resolves {
		if strings.TrimSpace(requirement) == "" {
			return errors.New("need resolves holds an empty entry")
		}
	}
	if err := validRetired(n.Retired); err != nil {
		return fmt.Errorf("need %w", err)
	}
	return nil
}

func (r Reply) validate() error {
	if r.Kind != "accept" && r.Kind != "reply" {
		return fmt.Errorf("reply kind %q must be accept or reply", r.Kind)
	}
	if _, _, err := ParseEdgeID(r.Edge); err != nil {
		return fmt.Errorf("reply edge: %w", err)
	}
	if r.Epoch <= 0 {
		return errors.New("reply epoch must be positive")
	}
	if r.At.IsZero() {
		return errors.New("reply at is required")
	}
	if strings.TrimSpace(r.Text) == "" {
		return errors.New("reply text is required")
	}
	for key, hash := range r.Answers {
		if key != "need" && key != "offer" {
			return fmt.Errorf("reply answers has unknown key %q", key)
		}
		if strings.TrimSpace(hash) == "" {
			return fmt.Errorf("reply answers.%s is required", key)
		}
	}
	if r.Kind == "accept" {
		if r.Reason != "" {
			return errors.New("accepted reply must not have a reason")
		}
		return nil
	}
	if _, valid := replyReasons[r.Reason]; !valid {
		return fmt.Errorf("reply reason %q is invalid", r.Reason)
	}
	return nil
}

func (e EdgeRecord) validate() error {
	source, target, err := ParseEdgeID(e.Edge)
	if err != nil {
		return fmt.Errorf("edge record edge: %w", err)
	}
	if e.Source != source || e.Target != target {
		return errors.New("edge record source and target must match edge")
	}
	if _, valid := edgeStatuses[e.Status]; !valid {
		return fmt.Errorf("edge record status %q is invalid", e.Status)
	}
	return nil
}

func (f Finding) validate() error {
	if strings.TrimSpace(f.ID) == "" {
		return errors.New("review finding id is required")
	}
	if f.Severity != "stop" && f.Severity != "owner" {
		return fmt.Errorf("review finding severity %q is invalid", f.Severity)
	}
	if strings.TrimSpace(f.Text) == "" {
		return errors.New("review finding text is required")
	}
	if f.Origin != OriginReview {
		return fmt.Errorf("review finding origin %q must be %s: a finding comes from the review of the node's own spec", f.Origin, OriginReview)
	}
	if strings.TrimSpace(f.Node) == "" {
		return errors.New("review finding node is required")
	}
	return nil
}

func (r Review) validate() error {
	if r.Subject != "spec" && r.Subject != "need" && r.Subject != "lean" && r.Subject != "migration" {
		return fmt.Errorf("review subject %q is invalid", r.Subject)
	}
	if r.Revision <= 0 || r.Pass <= 0 {
		return errors.New("review revision and pass must be positive")
	}
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("review model is required")
	}
	for _, finding := range r.Findings {
		if err := finding.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r UpstreamReview) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"contract", r.Contract}, {"reviewer", r.Reviewer}, {"upstream", r.Upstream}, {"head", r.Head}, {"from_hash", r.FromHash}, {"to_hash", r.ToHash},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("upstream review %s is required", field.name)
		}
	}
	if r.Verdict != "accept" && r.Verdict != "refuse" {
		return fmt.Errorf("upstream review verdict %q is invalid", r.Verdict)
	}
	if r.Verdict == "refuse" {
		if r.Reason == "" {
			return errors.New("refused upstream review reason is required")
		}
		if r.Reason != "program-defect" && r.Reason != "false-shared-assumption" && r.Reason != "proved-incompatible" {
			return fmt.Errorf("upstream review refusal reason %q is invalid", r.Reason)
		}
	} else if r.Reason != "" {
		return errors.New("accepted upstream review must not have a reason")
	}
	if r.At.IsZero() {
		return errors.New("upstream review at is required")
	}
	for _, pass := range r.Passes {
		if pass.Pass <= 0 || strings.TrimSpace(pass.Model) == "" {
			return errors.New("upstream review pass and model are required")
		}
		for _, finding := range pass.Findings {
			if err := finding.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n NodeMeta) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"id", n.ID}, {"key", n.Key}, {"kind", n.Kind}, {"role", n.Role}, {"lean_dir", n.LeanDir}, {"c_file", n.CFile},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("node metadata %s is required", field.name)
		}
	}
	return nil
}

func (e Escalation) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"subject", e.Subject}, {"record", e.Record}, {"reason", e.Reason},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("escalation %s is required", field.name)
		}
	}
	if e.Reason == "program-defect" && (strings.TrimSpace(e.Counterexample) == "" || strings.TrimSpace(e.BugsRow) == "") {
		return errors.New("program-defect escalation requires counterexample and bugs_row")
	}
	return nil
}

func (p Produced) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"interface", p.Interface}, {"module", p.Module}, {"declaration", p.Declaration}, {"migration", p.Migration},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("produced %s is required", field.name)
		}
	}
	return nil
}

// Marshal serializes the record as YAML after validating it.
func (n NeedFront) Marshal() ([]byte, error) { return marshalRecord(n, n.validate) }

// Unmarshal strictly decodes a YAML need record.
func (n *NeedFront) Unmarshal(data []byte) error { return yaml.Unmarshal(data, n) }

// Marshal serializes the record as YAML after validating it.
func (r Reply) Marshal() ([]byte, error) { return marshalRecord(r, r.validate) }

// Unmarshal strictly decodes a YAML reply record.
func (r *Reply) Unmarshal(data []byte) error { return yaml.Unmarshal(data, r) }

// Marshal serializes the record as YAML after validating it.
func (e EdgeRecord) Marshal() ([]byte, error) { return marshalRecord(e, e.validate) }

// Unmarshal decodes and validates a YAML edge record.
func (e *EdgeRecord) Unmarshal(data []byte) error {
	return unmarshalRecord(data, e, func() error { return e.validate() })
}

// Marshal serializes the record as YAML after validating it.
func (r Review) Marshal() ([]byte, error) { return marshalRecord(r, r.validate) }

// Unmarshal strictly decodes a YAML review record.
func (r *Review) Unmarshal(data []byte) error { return yaml.Unmarshal(data, r) }

// Marshal serializes the record as YAML after validating it.
func (r UpstreamReview) Marshal() ([]byte, error) { return marshalRecord(r, r.validate) }

// Unmarshal strictly decodes a YAML upstream review record.
func (r *UpstreamReview) Unmarshal(data []byte) error { return yaml.Unmarshal(data, r) }

// Marshal serializes the record as YAML after validating it.
func (n NodeMeta) Marshal() ([]byte, error) { return marshalRecord(n, n.validate) }

// Unmarshal decodes and validates YAML node metadata.
func (n *NodeMeta) Unmarshal(data []byte) error {
	return unmarshalRecord(data, n, func() error { return n.validate() })
}

// Marshal serializes the record as YAML after validating it.
func (e Escalation) Marshal() ([]byte, error) { return marshalRecord(e, e.validate) }

// Unmarshal decodes and validates a YAML escalation.
func (e *Escalation) Unmarshal(data []byte) error {
	return unmarshalRecord(data, e, func() error { return e.validate() })
}

// Marshal serializes the record as YAML after validating it.
func (p Produced) Marshal() ([]byte, error) { return marshalRecord(p, p.validate) }

// Unmarshal decodes and validates a YAML produced record.
func (p *Produced) Unmarshal(data []byte) error {
	return unmarshalRecord(data, p, func() error { return p.validate() })
}

func marshalRecord(value any, validate func() error) ([]byte, error) {
	if err := validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(value)
}

func unmarshalRecord(data []byte, dest any, validate func() error) error {
	if err := decodeStrictYAML(data, dest); err != nil {
		return err
	}
	return validate()
}

func (n NeedFront) MarshalJSON() ([]byte, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	type wire NeedFront
	return json.Marshal(wire(n))
}

func (n *NeedFront) UnmarshalJSON(data []byte) error {
	type wire NeedFront
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	value := NeedFront(decoded)
	if err := value.validate(); err != nil {
		return err
	}
	*n = value
	return nil
}

func (n NeedFront) MarshalYAML() (any, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	type wire NeedFront
	return wire(n), nil
}

func (n *NeedFront) UnmarshalYAML(value *yaml.Node) error {
	type wire NeedFront
	var decoded wire
	if err := decodeStrictYAMLNode(value, &decoded, "edge", "source", "target", "origin", "revision", "epoch", "cites", "resolves", "retired", "author_model"); err != nil {
		return err
	}
	parsed := NeedFront(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*n = parsed
	return nil
}

func (r Reply) MarshalJSON() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire Reply
	return json.Marshal(wire(r))
}

func (r *Reply) UnmarshalJSON(data []byte) error {
	type wire Reply
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	parsed := Reply(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func (r Reply) MarshalYAML() (any, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire Reply
	return wire(r), nil
}

func (r *Reply) UnmarshalYAML(value *yaml.Node) error {
	type wire Reply
	var decoded wire
	if err := decodeStrictYAMLNode(value, &decoded, "kind", "edge", "reason", "premise_owner", "answers", "epoch", "at", "text"); err != nil {
		return err
	}
	parsed := Reply(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func (r Review) MarshalJSON() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire Review
	return json.Marshal(wire(r))
}

func (r *Review) UnmarshalJSON(data []byte) error {
	type wire Review
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	parsed := Review(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func (r Review) MarshalYAML() (any, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire Review
	return wire(r), nil
}

// UnmarshalYAML strictly decodes one finding, so a field outside the record format is refused.
func (f *Finding) UnmarshalYAML(value *yaml.Node) error {
	type wire Finding
	var decoded wire
	if err := decodeStrictYAMLNode(value, &decoded, "id", "severity", "text", "resolved_by", "origin", "node"); err != nil {
		return err
	}
	*f = Finding(decoded)
	return nil
}

func (r *Review) UnmarshalYAML(value *yaml.Node) error {
	type wire Review
	var decoded wire
	if err := decodeStrictYAMLNode(value, &decoded, "subject", "revision", "pass", "model", "findings"); err != nil {
		return err
	}
	parsed := Review(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func (r UpstreamReview) MarshalJSON() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire UpstreamReview
	return json.Marshal(wire(r))
}

func (r *UpstreamReview) UnmarshalJSON(data []byte) error {
	type wire UpstreamReview
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	parsed := UpstreamReview(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func (r UpstreamReview) MarshalYAML() (any, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	type wire UpstreamReview
	return wire(r), nil
}

func (r *UpstreamReview) UnmarshalYAML(value *yaml.Node) error {
	type wire UpstreamReview
	var decoded wire
	if err := decodeStrictYAMLNode(value, &decoded, "contract", "reviewer", "upstream", "head", "from_hash", "to_hash", "verdict", "reason", "passes", "at"); err != nil {
		return err
	}
	parsed := UpstreamReview(decoded)
	if err := parsed.validate(); err != nil {
		return err
	}
	*r = parsed
	return nil
}

func decodeStrictJSON(data []byte, dest any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeStrictYAMLNode(node *yaml.Node, dest any, allowed ...string) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("record must be a YAML mapping")
	}
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if _, exists := known[key.Value]; !exists {
			return fmt.Errorf("field %q not found in record", key.Value)
		}
	}
	return node.Decode(dest)
}

// ContentHash returns the SHA-256 content hash format used by record references.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
