// Probe: a push with an open pull request.
package replay

import (
	json "encoding/json/v2"
	"fmt"
	"io"
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
	Repository  string       `json:"repository"` // a git repository holding every commit below
	Base        string       `json:"base"`       // 40 lowercase hex
	Members     []Member     `json:"members"`    // in the recorded merge order
	BuildRounds []BuildRound `json:"buildRounds"`
	Unresolved  []Unresolved `json:"unresolved,omitempty"`
}

type Member struct {
	ID   string `json:"id"`
	Head string `json:"head"` // 40 lowercase hex
}

type BuildRound struct {
	Round         int      `json:"round"`
	Excluded      []string `json:"excluded"`      // members removed before this build round
	Log           string   `json:"log"`           // this build round's recorded log, relative to the manifest
	RecordedKicks []Kick   `json:"recordedKicks"` // the recorded program's removals after this build round
}

type Kick struct {
	Member string `json:"member"`
	Reason string `json:"reason"`
}

type Unresolved struct {
	ID  string `json:"id"`
	Why string `json:"why"`
}

// Load reads and checks a manifest. It reads the manifest and its logs through one handle on the directory the
// operating system resolves for the path's directory part, uncleaned, so the file it reads is the one the path names
// (`link/..` included) and its logs are checked beside that file. The JSON must spell every key exactly once and
// exactly as the format does: a repeated key, a key in another case, an unknown key or a trailing value is refused.
func Load(path string) (Manifest, error) {
	dir, name := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest directory: %w", err)
	}
	defer root.Close()
	file, err := root.Open(name)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest: %w", err)
	}
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(data, &manifest, json.RejectUnknownMembers(true)); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.validate(root); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) validate(root *os.Root) error {
	if m.Format != Format {
		return fmt.Errorf("manifest format %q, want %q", m.Format, Format)
	}
	if !commitID.MatchString(m.Base) {
		return fmt.Errorf("manifest base %q is not a 40-character lowercase hexadecimal commit", m.Base)
	}
	if len(m.BuildRounds) == 0 {
		return fmt.Errorf("manifest has no build rounds")
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
	for _, unresolved := range m.Unresolved {
		if _, exists := members[unresolved.ID]; exists {
			return fmt.Errorf("manifest repeats member %q as unresolved", unresolved.ID)
		}
		members[unresolved.ID] = struct{}{}
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
		if err := validateLog(root, round.Log); err != nil {
			return fmt.Errorf("manifest build round %d log %q: %w", round.Round, round.Log, err)
		}
	}
	return nil
}

func validateLog(root *os.Root, log string) error {
	clean := filepath.Clean(log)
	if filepath.IsAbs(log) || log == "" || clean != log {
		return fmt.Errorf("is not a clean relative path")
	}
	parts := strings.Split(log, string(filepath.Separator))
	current := ""
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("is not a clean relative path")
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("contains a symbolic link")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("has a non-directory path component")
		}
	}
	info, err := root.Stat(log)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("is not a regular file")
	}
	return nil
}
