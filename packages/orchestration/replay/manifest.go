package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const Format = "landing-replay/v1"

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Manifest struct {
	Format      string       `json:"format"`
	Name        string       `json:"name"`
	Repository  string       `json:"repository"`
	Base        string       `json:"base"`
	Members     []Member     `json:"members"`
	BuildRounds []BuildRound `json:"buildRounds"`
	Unresolved  []Unresolved `json:"unresolved,omitempty"`
}

type Member struct {
	ID   string `json:"id"`
	Head string `json:"head"`
}

type BuildRound struct {
	Round         int      `json:"round"`
	Excluded      []string `json:"excluded"`
	Log           string   `json:"log"`
	RecordedKicks []Kick   `json:"recordedKicks"`
}

type Kick struct {
	Member string `json:"member"`
	Reason string `json:"reason"`
}

type Unresolved struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

func Load(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()

	var manifest Manifest
	if err := json.NewDecoder(file).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.validate(filepath.Dir(path)); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) validate(dir string) error {
	if m.Format != Format {
		return fmt.Errorf("manifest format %q, want %q", m.Format, Format)
	}
	if !commitID.MatchString(m.Base) {
		return fmt.Errorf("manifest base %q is not a 40-character lowercase hexadecimal commit", m.Base)
	}

	members := make(map[string]struct{}, len(m.Members))
	for _, member := range m.Members {
		if _, exists := members[member.ID]; exists {
			return fmt.Errorf("manifest repeats member %q", member.ID)
		}
		if !commitID.MatchString(member.Head) {
			return fmt.Errorf("manifest member %q head %q is not a 40-character lowercase hexadecimal commit", member.ID, member.Head)
		}
		members[member.ID] = struct{}{}
	}

	for index, round := range m.BuildRounds {
		if round.Round != index+1 {
			return fmt.Errorf("manifest build round %d has number %d, want %d", index+1, round.Round, index+1)
		}
		for _, id := range round.Excluded {
			if _, exists := members[id]; !exists {
				return fmt.Errorf("manifest build round %d excludes unknown member %q", round.Round, id)
			}
		}
		for _, kick := range round.RecordedKicks {
			if _, exists := members[kick.Member]; !exists {
				return fmt.Errorf("manifest build round %d records a kick for unknown member %q", round.Round, kick.Member)
			}
		}
		log := filepath.Clean(round.Log)
		if filepath.IsAbs(round.Log) || log == "." || log == ".." || strings.HasPrefix(log, ".."+string(filepath.Separator)) {
			return fmt.Errorf("manifest build round %d log %q is not relative to the manifest", round.Round, round.Log)
		}
		if _, err := os.Stat(filepath.Join(dir, log)); err != nil {
			return fmt.Errorf("manifest build round %d log %q: %w", round.Round, round.Log, err)
		}
	}
	return nil
}
