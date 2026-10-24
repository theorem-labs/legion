package owner

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// InboxItem is one open item of an owner's queue with the record that explains it. RecordOwner is the owner
// whose branch holds Record; Branch names a branch that is no owner's (LibrarianInboxBranch); a Record with neither
// means main. Event is the ledger event a returned item reads. Commit is filled by the caller from the branch. Ready
// is false for an item whose input is not there yet: a concept item before its record is on its branch
// (WaitsForRecord), which the caller decides once it read the branch.
type InboxItem struct {
	ledger.Item
	Record              string `json:"record"`
	RecordOwner         string `json:"record_owner"`
	Branch              string `json:"branch,omitempty"`
	Event               int64  `json:"event,omitempty"`
	Commit              string `json:"commit"`
	Ready               bool   `json:"ready"`
	StoppedInstructions int    `json:"stopped_instructions"`
	CallHeight          int    `json:"call_height"`
}

// WaitsForRecord reports whether the item is ready only once its record is on its branch: a concept item, whose
// record the gate (near: the verdict beside the proposal on the proposer's branch) or the intake (recheck: the
// envelope on LibrarianInboxBranch) writes.
func (i InboxItem) WaitsForRecord() bool { return strings.HasPrefix(i.ID, "concept:") }

// BuildInbox names each item's record from config and orders the items by rule. items are an owner's open items in
// the ledger's order (Store.OpenItems), which is the order it enqueued them; items the rule's keys tie keep it.
func BuildInbox(items []ledger.Item, config Config, priorities map[string]Priority, rule Rule) ([]InboxItem, error) {
	byID := make(map[string]InboxItem, len(items))
	order := make([]PrioritizedItem, 0, len(items))
	for position, item := range items {
		entry, err := inboxEntry(item, config)
		if err != nil {
			return nil, err
		}
		priority := priorities[item.ID]
		entry.StoppedInstructions, entry.CallHeight = priority.StoppedInstructions, priority.CallHeight
		byID[item.ID] = entry
		order = append(order, PrioritizedItem{
			ID: item.ID, StoppedInstructions: priority.StoppedInstructions, CallHeight: priority.CallHeight, Position: position,
		})
	}
	rule.Sort(order)
	inbox := make([]InboxItem, 0, len(order))
	for _, item := range order {
		inbox = append(inbox, byID[item.ID])
	}
	return inbox, nil
}

// NodeRecord is the path of one record in an owner's node directory.
func NodeRecord(recordsRoot, owner string, elems ...string) string {
	return path.Join(append([]string{recordsRoot, "nodes", Key(owner)}, elems...)...)
}

// NeedRecord is the path of source's need to target.
func NeedRecord(recordsRoot, source, target string) string {
	return NodeRecord(recordsRoot, source, "needs", "E"+Hex16(target)+".md")
}

// SpecRecord is the path of an owner's spec.
func SpecRecord(recordsRoot, owner string) string { return NodeRecord(recordsRoot, owner, "spec.md") }

// ReplyRecord is the path of the n-th reply about edge in the replier's node.
func ReplyRecord(recordsRoot, replier, edge string, n int) string {
	return NodeRecord(recordsRoot, replier, "replies", Hex16(edge)+"-"+strconv.Itoa(n)+".yaml")
}

// inboxEntry names the record an item asks its owner to read. A reply item's record is found by its
// content hash in the parties' replies, so it is left for the caller; items whose input is not an owner
// record (interfaces, scores, upstream merges, proposals, escalations) name none. A concept item names the record it
// waits for, and is not ready until the caller finds it.
func inboxEntry(item ledger.Item, config Config) (InboxItem, error) {
	entry := InboxItem{Item: item}
	entry.Ready = !entry.WaitsForRecord()
	recordsRoot := config.RecordsRoot
	ref, err := ParseItemID(item.ID)
	if err != nil {
		return InboxItem{}, err
	}
	switch ref.Type {
	case "start":
		entry.Record, entry.RecordOwner = SpecRecord(recordsRoot, ref.Subject), ref.Subject
	case "need":
		source, target, err := ParseEdgeID(ref.Subject)
		if err != nil {
			return InboxItem{}, err
		}
		entry.Record, entry.RecordOwner = NeedRecord(recordsRoot, source, target), source
	case "offer":
		_, target, err := ParseEdgeID(ref.Subject)
		if err != nil {
			return InboxItem{}, err
		}
		entry.Record, entry.RecordOwner = SpecRecord(recordsRoot, target), target
	case "agreed", "lean", "stale":
		edge := ref.Subject
		if ref.Type == "stale" {
			edge = strings.TrimPrefix(edge, "edge:")
			edge = edge[:strings.LastIndexByte(edge, '@')]
		}
		source, target, err := ParseEdgeID(edge)
		if err != nil {
			return InboxItem{}, err
		}
		if config.Paths.EdgeRecordTemplate == "" {
			return InboxItem{}, fmt.Errorf("owner configuration has no paths.edge_record")
		}
		entry.Record, entry.RecordOwner = config.Paths.EdgeRecord(source, target), source
	case "split-review":
		entry.Record, entry.RecordOwner = SpecRecord(recordsRoot, ref.Subject), ref.Subject
	case "rebuild":
		entry.Record = ref.Subject
	case "returned":
		event, err := strconv.ParseInt(ref.Version, 10, 64)
		if err != nil {
			return InboxItem{}, fmt.Errorf("returned item %q event: %w", item.ID, err)
		}
		entry.Event = event
	case "concept":
		if digest, near := strings.CutPrefix(ref.Version, "near-"); near {
			entry.Record, entry.RecordOwner = NodeRecord(recordsRoot, ref.Subject, "concepts", digest+".verdict.json"), ref.Subject
		} else {
			entry.Record, entry.Branch = LibrarianInboxRecord(ref.Version), LibrarianInboxBranch
		}
	}
	return entry, nil
}

// ReturnFields are the C13 fields an owner reads to repair a returned migration. The producer (`ledger round`)
// requires the reason and the key, and writes each of batch, detail and log that it was not given as an empty
// string; such a field is absent here, and left out of the JSON.
type ReturnFields struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	Log    string `json:"log,omitempty"`
	Batch  string `json:"batch,omitempty"`
	Key    string `json:"key"`
}

// ReturnFromEvent reads the C13 fields of a migration.round_failed event and refuses an event without a reason or a
// key, which the producer never writes.
func ReturnFromEvent(event ledger.Event) (ReturnFields, error) {
	if event.Kind != "migration.round_failed" {
		return ReturnFields{}, fmt.Errorf("event %d is %s, not migration.round_failed", event.ID, event.Kind)
	}
	var fields ReturnFields
	if err := json.Unmarshal(event.Data, &fields); err != nil {
		return ReturnFields{}, fmt.Errorf("decode event %d: %w", event.ID, err)
	}
	for _, field := range []struct{ name, value string }{{"reason", fields.Reason}, {"key", fields.Key}} {
		if strings.TrimSpace(field.value) == "" {
			return ReturnFields{}, fmt.Errorf("event %d lacks the C13 field %s", event.ID, field.name)
		}
	}
	return fields, nil
}
