// Package conceptgate keeps a project's registry of shared concepts and the similarity gate in front of its
// librarian.
package conceptgate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// RegistryVersion is the registry directory's format version.
const RegistryVersion = 2

// Concept is one registered shared concept.
type Concept struct {
	Name         string   `yaml:"name" json:"name"`
	Home         string   `yaml:"home" json:"home"`
	Meaning      string   `yaml:"meaning" json:"meaning"`
	Extends      string   `yaml:"extends,omitempty" json:"extends,omitempty"`
	Forms        []string `yaml:"forms,omitempty" json:"forms,omitempty"`
	Proposal     string   `yaml:"proposal,omitempty" json:"proposal,omitempty"`
	Proposer     string   `yaml:"proposer,omitempty" json:"proposer,omitempty"`
	CompiledHash string   `yaml:"compiled_hash,omitempty" json:"compiled_hash,omitempty"`
	AcceptedBy   string   `yaml:"accepted_by,omitempty" json:"accepted_by,omitempty"`
	AcceptedNear []string `yaml:"accepted_near,omitempty" json:"accepted_near,omitempty"`
	Source       string   `yaml:"source,omitempty" json:"source,omitempty"`
}

// Registry is the registry directory's registry.yaml and its concept files.
type Registry struct {
	Version int      `yaml:"version"`
	Imports []string `yaml:"imports,omitempty"`
	Opens   []string `yaml:"opens,omitempty"`

	Concepts []Concept `yaml:"-" json:"-"`
	// Refused are the concept files that Load refused and left out of Concepts.
	Refused []RefusedFile `yaml:"-" json:"-"`
}

// RefusedFile is a concept file that Load refused: its registry-relative path, the defect that says why, and the
// name and compiled hash that its YAML gives, when it decodes that far. The file still holds its name and its path
// in the registry, so the gate adds no concept that would take either (Gate.Decide).
type RefusedFile struct {
	Path         string
	Name         string
	CompiledHash string
	Defect       Defect
}

// Key is the refused file's name without its concepts/ prefix or .yaml suffix.
func (r RefusedFile) Key() string {
	return strings.TrimSuffix(strings.TrimPrefix(r.Path, "concepts/"), ".yaml")
}

// Defect is a registry inconsistency that does not prevent the gate or refresh from using the registry.
type Defect struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (d Defect) Error() string {
	if d.Path == "" {
		return d.Message
	}
	return d.Path + ": " + d.Message
}

// sentenceBreak finds the end of a sentence before the end of the text: a full stop, question or exclamation
// mark followed by a space and a capital letter.
var (
	sentenceBreak = regexp.MustCompile(`[.!?]\s+\p{Lu}`)
	statementHash = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func (c Concept) check() error {
	switch {
	case strings.TrimSpace(c.Name) == "" || strings.ContainsAny(c.Name, " \t\n"):
		return fmt.Errorf("concept %q: the name is one Lean declaration name", c.Name)
	case strings.TrimSpace(c.Home) == "":
		return fmt.Errorf("concept %s has no home file", c.Name)
	case strings.TrimSpace(c.Meaning) == "":
		return fmt.Errorf("concept %s has no meaning", c.Name)
	case strings.Contains(c.Meaning, "\n") || sentenceBreak.MatchString(c.Meaning):
		return fmt.Errorf("concept %s: its meaning is one sentence", c.Name)
	case c.CompiledHash != "" && !statementHash.MatchString(c.CompiledHash):
		return fmt.Errorf("concept %s: compiled hash is not 32 lowercase hexadecimal characters", c.Name)
	}
	return nil
}

// Key is the name of a concept's file without its concepts/ prefix or .yaml suffix.
func Key(c Concept) string {
	if c.CompiledHash != "" {
		return c.CompiledHash
	}
	return c.Name
}

// checkForms refuses a form that is not one lambda term.
func (c Concept) checkForms() error {
	return declindex.FormsModule{Concepts: []declindex.ConceptForms{{Concept: c.Name, Forms: c.Forms}}}.Check()
}

// Render returns the canonical YAML for one concept file.
func Render(c Concept) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if err := c.checkForms(); err != nil {
		return nil, err
	}
	return yaml.Marshal(c)
}

// decodeConcept reads one concept file: known keys only, and a concept that passes its checks.
func decodeConcept(raw []byte) (Concept, error) {
	var c Concept
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Concept{}, err
	}
	if err := c.check(); err != nil {
		return Concept{}, err
	}
	if err := c.checkForms(); err != nil {
		return Concept{}, err
	}
	return c, nil
}

// Load reads a format-2 registry directory. Defects describe the concept files that the loader refuses (YAML that
// does not decode, an unknown key, a concept that fails its checks), which it leaves out, and duplicate names and
// compiled hashes, wrongly keyed files and extends of concepts that are not registered; they do not stop callers
// from checking the valid entries. Only a registry.yaml that is missing, malformed or of another format, or a file
// that cannot be read, is an error.
func Load(dir string) (Registry, []Defect, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Registry{}, nil, err
	}
	if !info.IsDir() {
		return Registry{}, nil, fmt.Errorf("%s: registry format %d is a directory", dir, RegistryVersion)
	}
	root := filepath.Join(dir, "registry.yaml")
	raw, err := os.ReadFile(root)
	if err != nil {
		return Registry{}, nil, err
	}
	var header struct {
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &header); err != nil {
		return Registry{}, nil, fmt.Errorf("%s: %w", root, err)
	}
	if header.Version != RegistryVersion {
		return Registry{}, nil, fmt.Errorf("%s: registry format %d; this gate reads format %d", root, header.Version, RegistryVersion)
	}
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return Registry{}, nil, fmt.Errorf("%s: %w", root, err)
	}
	concepts := filepath.Join(dir, "concepts")
	entries, err := os.ReadDir(concepts)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil, nil
		}
		return Registry{}, nil, err
	}
	var defects []Defect
	names := map[string]string{}
	hashes := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		relative := filepath.ToSlash(filepath.Join("concepts", entry.Name()))
		raw, err := os.ReadFile(filepath.Join(concepts, entry.Name()))
		if err != nil {
			return Registry{}, nil, err
		}
		c, err := decodeConcept(raw)
		if err != nil {
			defect := Defect{Path: relative, Message: err.Error()}
			defects = append(defects, defect)
			// Whatever name and hash the file gives, read without its checks; YAML that does not decode gives none.
			var held struct {
				Name         string `yaml:"name"`
				CompiledHash string `yaml:"compiled_hash"`
			}
			_ = yaml.Unmarshal(raw, &held)
			r.Refused = append(r.Refused, RefusedFile{Path: relative, Name: held.Name, CompiledHash: held.CompiledHash, Defect: defect})
			continue
		}
		expected := filepath.ToSlash(filepath.Join("concepts", Key(c)+".yaml"))
		if relative != expected {
			defects = append(defects, Defect{Path: relative, Message: "file is named " + relative + ", want " + expected})
		}
		if previous, ok := names[c.Name]; ok {
			defects = append(defects, Defect{Path: relative, Message: "concept " + c.Name + " is also in " + previous})
		} else {
			names[c.Name] = relative
		}
		if c.CompiledHash != "" {
			if previous, ok := hashes[c.CompiledHash]; ok {
				defects = append(defects, Defect{Path: relative, Message: "compiled hash " + c.CompiledHash + " is also in " + previous})
			} else {
				hashes[c.CompiledHash] = relative
			}
		}
		r.Concepts = append(r.Concepts, c)
	}
	for _, c := range r.Concepts {
		if c.Extends != "" {
			if _, ok := names[c.Extends]; !ok {
				defects = append(defects, Defect{Path: filepath.ToSlash(filepath.Join("concepts", Key(c)+".yaml")), Message: "concept " + c.Name + " extends " + c.Extends + ", which is not registered"})
			}
		}
	}
	return r, defects, nil
}

// Find gives the concept of a name.
func (r Registry) Find(name string) (Concept, bool) {
	for _, c := range r.Concepts {
		if c.Name == name {
			return c, true
		}
	}
	return Concept{}, false
}
