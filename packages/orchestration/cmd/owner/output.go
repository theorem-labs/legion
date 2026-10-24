package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func text(value any) string {
	switch v := value.(type) {
	case whoamiResult:
		return fmt.Sprintf("owner %s\nrole %s\nkey %s\nkind %s\nlean_dir %s\nc_file %s\nactive_set %s\nactive %t\nepoch %d",
			v.Owner, v.Role, v.Key, v.Kind, v.LeanDir, v.CFile, v.ActiveSet, v.Active, v.Epoch)
	case topicsResult:
		return strings.Join(v.Topics, "\n")
	case worktreeResult:
		return v.Path + " " + v.Branch
	case []ownerpkg.InboxItem:
		lines := make([]string, 0, len(v))
		for _, item := range v {
			line := strings.Join([]string{item.ID, item.Record, item.Commit}, "\t")
			if !item.Ready {
				line += "\tnot ready"
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	case ownerpkg.ReturnFields:
		return fmt.Sprintf("reason %s\ndetail %s\nlog %s\nbatch %s\nkey %s", v.Reason, absent(v.Detail), absent(v.Log), absent(v.Batch), v.Key)
	case stepResult:
		state := "done"
		if v.AlreadyDone {
			state = "already done"
		}
		line := fmt.Sprintf("%s: %s at %s", state, v.Record, v.Commit)
		if len(v.FollowUps) > 0 {
			line += "; queued " + strings.Join(v.FollowUps, ", ")
		}
		return line
	case ledger.Lease:
		return fmt.Sprintf("%s held by %s (generation %d)", v.ID, v.Holder, v.Generation)
	case ledger.Item:
		return fmt.Sprintf("%s %s %s", v.ID, v.State, v.Result)
	case messageTestResult:
		return "message test passed: report " + v.Report
	case intakeResult:
		state := "queued"
		if !v.Added {
			state = "already queued"
		}
		line := fmt.Sprintf("%s %s for %s", state, v.Item, ownerpkg.Librarian)
		if v.Record != "" {
			line += fmt.Sprintf("; record %s at %s", v.Record, v.Commit)
		}
		return line
	}
	data, _ := json.Marshal(value)
	return string(data)
}

// absent is an optional field as text: its value, or "(absent)" when it has none.
func absent(value string) string {
	if value == "" {
		return "(absent)"
	}
	return value
}
