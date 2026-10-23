package ledger

import "time"

// Repository is the scope every record belongs to: one git repository that many agents change.
type Repository struct {
	Name string `json:"name"`
	// MaxLandingRounds is how many failed landing rounds a migration may use before it goes to the steward.
	// Zero turns escalation off.
	MaxLandingRounds int       `json:"max_landing_rounds"`
	CreatedAt        time.Time `json:"created_at"`
}

// Item is a work item and its lease. Holder is empty when nobody holds it.
type Item struct {
	ID         string     `json:"id"`
	State      string     `json:"state"` // open | done
	Holder     string     `json:"holder,omitempty"`
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Result     string     `json:"result,omitempty"`
}

// Lease is a granted or renewed lease.
type Lease struct {
	ID         string    `json:"id"`
	Holder     string    `json:"holder"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// PathClash is one overlap between two write sets: Ours covers or is covered by Theirs.
type PathClash struct {
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}
