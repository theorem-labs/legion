package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// OwnerRefusal is one standing owner the daemon will not start while its work is unchanged: an
// owner key outside the owner-key grammar, or a node kind with no template. Revision is the ledger
// work revision it was refused at; Recorded is whether the ledger has the durable refusal
// (RecordOwnerRefused), which the daemon retries until it does.
type OwnerRefusal struct {
	Repository string    `json:"repository"`
	Owner      string    `json:"owner"`
	Revision   int64     `json:"revision"`
	Reason     string    `json:"reason"`
	RefusedAt  time.Time `json:"refusedAt"`
	Recorded   bool      `json:"recorded"`
}

// OwnerRefusals is GET /legion/v1/operator/owners/refused's answer, in owner order.
type OwnerRefusals struct {
	Refused []OwnerRefusal `json:"refused"`
}

// MarshalJSON keeps `refused` an array on the wire, empty rather than `null`.
func (r OwnerRefusals) MarshalJSON() ([]byte, error) {
	type wire OwnerRefusals
	out := wire(r)
	if out.Refused == nil {
		out.Refused = []OwnerRefusal{}
	}
	return json.Marshal(out)
}

// refusedOwners answers the standing owners the daemon refused, as the admission loop holds them
// (Options.RefusedOwners); none when the daemon runs no standing owners.
func (s *server) refusedOwners(w http.ResponseWriter, _ *http.Request) {
	var refused []OwnerRefusal
	if s.ownerRefusals != nil {
		refused = s.ownerRefusals()
	}
	writeJSON(w, http.StatusOK, OwnerRefusals{Refused: refused})
}
