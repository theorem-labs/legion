package main

import (
	"encoding/json"
	"fmt"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

func text(value any) string {
	switch v := value.(type) {
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
