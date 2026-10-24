package owner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Hex16 is the first 16 lowercase hexadecimal characters of SHA-256 over s's UTF-8 bytes.
func Hex16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// Key is the stable filesystem key for an owner id.
func Key(id string) string { return "K" + Hex16(id) }

// EdgeID is the stable identifier for a directed edge.
func EdgeID(source, target string) string { return source + "->" + target }

// ParseEdgeID splits an edge identifier at its only arrow.
func ParseEdgeID(edge string) (source, target string, err error) {
	if strings.Count(edge, "->") != 1 {
		return "", "", fmt.Errorf("edge %q must contain exactly one ->", edge)
	}
	source, target, _ = strings.Cut(edge, "->")
	if source == "" || target == "" {
		return "", "", fmt.Errorf("edge %q has an empty endpoint", edge)
	}
	return source, target, nil
}

// TopicToken preserves graph-token bytes and percent-encodes every other UTF-8 byte.
func TopicToken(id string) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(id))
	for _, b := range []byte(id) {
		if ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9') || b == '_' || b == ':' || b == '-' {
			out.WriteByte(b)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[b>>4])
		out.WriteByte(hexDigits[b&0x0f])
	}
	return out.String()
}

// ItemRef is the parsed form of a work-item identifier.
type ItemRef struct {
	Type     string
	Upstream string
	Subject  string
	Version  string
}

var versionedItemTypes = map[string]struct{}{
	"start": {}, "need": {}, "offer": {}, "reply": {}, "agreed": {}, "lean": {}, "returned": {},
	"rebuild": {}, "produce": {}, "interface": {}, "concept": {}, "concept-answer": {}, "split-review": {},
}

// String renders r in the work-item grammar.
func (r ItemRef) String() string {
	switch r.Type {
	case "upstream":
		return "upstream:" + r.Upstream + ":" + r.Subject
	case "upstream-review":
		id := "upstream-review:" + r.Upstream + ":" + r.Subject
		if r.Version != "" {
			id += "@" + r.Version
		}
		return id
	}
	if _, ok := versionedItemTypes[r.Type]; ok {
		return r.Type + ":" + r.Subject + "@" + r.Version
	}
	return r.Type + ":" + r.Subject
}

// ParseItemID decodes and validates a work-item identifier without confusing an owner's @ with its final version separator.
func ParseItemID(id string) (ItemRef, error) {
	typ, body, found := strings.Cut(id, ":")
	if !found || typ == "" || body == "" {
		return ItemRef{}, invalidItemID(id)
	}
	switch typ {
	case "upstream":
		upstream, subject, found := strings.Cut(body, ":")
		if !found || !lowerHex(upstream, 12) || !lowerHex(subject, 16) {
			return ItemRef{}, invalidItemID(id)
		}
		return ItemRef{Type: typ, Upstream: upstream, Subject: subject}, nil
	case "upstream-review":
		upstream, subject, found := strings.Cut(body, ":")
		if !found || !lowerHex(upstream, 12) || subject == "" {
			return ItemRef{}, invalidItemID(id)
		}
		// Only a final @<n> with n >= 2 is a review version; any other @ belongs to the contract name.
		version := ""
		if at := strings.LastIndexByte(subject, '@'); at > 0 && atLeast(subject[at+1:], 2) {
			version, subject = subject[at+1:], subject[:at]
		}
		return ItemRef{Type: typ, Upstream: upstream, Subject: subject, Version: version}, nil
	case "stale":
		if !validAgreement(body) {
			return ItemRef{}, invalidItemID(id)
		}
		return ItemRef{Type: typ, Subject: body}, nil
	case "proposal":
		if strings.Contains(body, "@") {
			return ItemRef{}, invalidItemID(id)
		}
		return ItemRef{Type: typ, Subject: body}, nil
	case "escalation", "decision":
		if !lowerHex(body, 16) {
			return ItemRef{}, invalidItemID(id)
		}
		return ItemRef{Type: typ, Subject: body}, nil
	}
	if _, ok := versionedItemTypes[typ]; !ok {
		return ItemRef{}, fmt.Errorf("unknown work item type %q", typ)
	}
	subject, version, err := splitItemVersion(id, body)
	if err != nil || !validVersionedItem(typ, subject, version) {
		return ItemRef{}, invalidItemID(id)
	}
	return ItemRef{Type: typ, Subject: subject, Version: version}, nil
}

// validVersionedItem checks a versioned item's layout. splitItemVersion has already refused an empty subject or version.
func validVersionedItem(typ, subject, version string) bool {
	switch typ {
	case "start":
		return version == "1"
	case "need", "offer":
		return validEdge(subject) && revision(version)
	case "split-review":
		return revision(version)
	case "reply", "lean":
		return validEdge(subject) && lowerHex(version, 12)
	case "agreed":
		return validEdge(subject) && positiveDecimal(version)
	case "returned":
		return validMigration(subject) && positiveDecimal(version)
	case "rebuild", "produce":
		return lowerHex(version, 12)
	case "concept", "concept-answer":
		return ValidOwnerID(subject) && GateIDKind(version) != ""
	case "interface":
		return lowerHex(subject, 16) && version == "1"
	}
	return false
}

func validAgreement(value string) bool {
	if !strings.HasPrefix(value, "edge:") {
		return false
	}
	edge, revision, err := splitItemVersion(value, strings.TrimPrefix(value, "edge:"))
	return err == nil && validEdge(edge) && positiveDecimal(revision)
}

func validMigration(value string) bool {
	if !strings.HasPrefix(value, "m:") {
		return false
	}
	key, generation, found := strings.Cut(strings.TrimPrefix(value, "m:"), ":")
	return found && len(key) == 17 && key[0] == 'K' && lowerHex(key[1:], 16) && positiveDecimal(generation)
}

func validEdge(value string) bool {
	_, _, err := ParseEdgeID(value)
	return err == nil
}

// The concept gate's owner-id rule (conceptgate/gate.go:85-96): a node id of the supervisor's node kinds, whose name
// has no white space and may hold @, or a role name.
var (
	nodeOwnerID = regexp.MustCompile(`^(function|struct|global|percpu|subsystem):\S+$`)
	roleOwnerID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// ValidOwnerID reports whether id passes the concept gate's owner-id rule, as a proposer must.
func ValidOwnerID(id string) bool { return nodeOwnerID.MatchString(id) || roleOwnerID.MatchString(id) }

// GateIDKind is the kind of a concept gate id: "near" for near-<hex16 of the proposal bytes> (conceptgate/gate.go:164-165),
// "recheck" for recheck-<key>-<key> (conceptgate/recheck.go:121-131), each key a compiled hash (32 lowercase hex digits)
// or name. and the lowercase hex of a concept name's bytes; "" for any other text. A gate id holds no @, so it is the
// version of a concept item, after the item's last @.
func GateIDKind(id string) string {
	if digest, found := strings.CutPrefix(id, "near-"); found {
		if lowerHex(digest, 16) {
			return "near"
		}
		return ""
	}
	if keys, found := strings.CutPrefix(id, "recheck-"); found {
		added, other, found := strings.Cut(keys, "-")
		if found && conceptKey(added) && conceptKey(other) {
			return "recheck"
		}
	}
	return ""
}

// conceptKey is a key of a recheck id: a compiled hash, or name. and the hex of the bytes of a name.
func conceptKey(key string) bool {
	if lowerHex(key, 32) {
		return true
	}
	name, found := strings.CutPrefix(key, "name.")
	return found && name != "" && len(name)%2 == 0 && lowerHex(name, len(name))
}

func revision(value string) bool {
	return len(value) > 1 && value[0] == 'r' && positiveDecimal(value[1:])
}

// ParseRevision reads a revision r<n>, written canonically: n is a positive decimal with no leading zero, so one
// revision has one spelling and one item id.
func ParseRevision(value string) (int, error) {
	if !revision(value) {
		return 0, fmt.Errorf("revision %q is not r<n> with n a positive decimal without a leading zero", value)
	}
	n, err := strconv.Atoi(value[1:])
	if err != nil {
		return 0, fmt.Errorf("revision %q: %w", value, err)
	}
	return n, nil
}

func atLeast(value string, minimum int) bool {
	if !positiveDecimal(value) {
		return false
	}
	n := 0
	for i := range value {
		n = n*10 + int(value[i]-'0')
		if n >= minimum {
			return true
		}
	}
	return false
}

// positiveDecimal accepts the canonical spelling of a positive integer: digits, the first of them not 0.
func positiveDecimal(value string) bool {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for i := range value {
		if !('0' <= value[i] && value[i] <= '9') && !('a' <= value[i] && value[i] <= 'f') {
			return false
		}
	}
	return true
}

func invalidItemID(id string) error { return fmt.Errorf("invalid work item %q", id) }

func splitItemVersion(id, body string) (subject, version string, err error) {
	at := strings.LastIndexByte(body, '@')
	if at <= 0 || at == len(body)-1 {
		return "", "", fmt.Errorf("item %q needs a subject and version", id)
	}
	return body[:at], body[at+1:], nil
}
