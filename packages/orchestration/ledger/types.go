package ledger

import "time"

// Repository is the scope every record belongs to: one git repository that many agents change.
type Repository struct {
	Name string `json:"name"`
	// MaxLandingRounds is how many failed landing rounds a migration may use before it goes to the steward.
	// Zero turns escalation off.
	MaxLandingRounds int       `json:"max_landing_rounds"`
	UpstreamOwner    string    `json:"upstream_owner,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Item is a work item and its lease. Owner is the standing owner whose queue carries it; Holder is
// empty when nobody holds it. ExpiresAt is nil when nobody holds it and for a supervised lease, which
// has no end.
type Item struct {
	ID         string     `json:"id"`
	Owner      string     `json:"owner,omitempty"`
	State      string     `json:"state"` // open | done
	Holder     string     `json:"holder,omitempty"`
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Result     string     `json:"result,omitempty"`
}

// Lease is a granted or renewed lease. ExpiresAt is nil for a supervised lease, which has no end.
type Lease struct {
	ID         string     `json:"id"`
	Holder     string     `json:"holder"`
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// PathClash is one overlap between two write sets: Ours covers or is covered by Theirs.
type PathClash struct {
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}

// LastChange is the record that allowed a contract's most recent statement advance: the compatibility proof or the
// owner's review that cleared it, with the upstream commit beside it when the landing carried one.
type LastChange struct {
	Proof    string  `json:"proof,omitempty"`
	Review   *Review `json:"review,omitempty"`
	Upstream string  `json:"upstream,omitempty"`
}

// Contract records who owns a statement and the version of its current hash.
type Contract struct {
	Name          string      `json:"name"`
	Owner         string      `json:"owner"`
	StatementHash string      `json:"statement_hash"`
	Version       int64       `json:"version"`
	Pinned        bool        `json:"pinned"`
	LastChange    *LastChange `json:"last_change"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Agreement records the versions of contracts that its parties agreed to use.
type Agreement struct {
	ID        string           `json:"id"`
	Parties   []string         `json:"parties"`
	State     string           `json:"state"`
	CreatedAt time.Time        `json:"created_at"`
	StaledAt  *time.Time       `json:"staled_at,omitempty"`
	Reads     map[string]int64 `json:"reads"`
}
