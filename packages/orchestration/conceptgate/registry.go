// Package conceptgate keeps a project's registry of shared concepts and the similarity gate in front of its
// librarian. The registry is a YAML file in the project's git: for each shared concept one name (a Lean
// declaration), one home file, a meaning in one sentence, the concept it extends if any, and the forms that state
// its fact. The gate compares a proposed concept with the registry, by statement hash and by compiled statement:
// it refuses an exact duplicate and names the existing concept, sends a near match to the librarian, and adds the
// rest to the registry.
package conceptgate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/orchestration/declindex"
)

// RegistryVersion is the registry file's format version.
const RegistryVersion = 1

// Concept is one registered shared concept.
type Concept struct {
	// Name is the concept's Lean declaration (or the name it will have when its home file declares it).
	Name string `yaml:"name" json:"name"`
	// Home is the file that declares it, relative to the project's root.
	Home string `yaml:"home" json:"home"`
	// Meaning says what it is, in one sentence.
	Meaning string `yaml:"meaning" json:"meaning"`
	// Extends names the registered concept it extends, if any.
	Extends string `yaml:"extends,omitempty" json:"extends,omitempty"`
	// Forms are the ways to state its fact, Lean lambda terms whose binders are pattern variables. With none,
	// its declaration's body is its form.
	Forms []string `yaml:"forms,omitempty" json:"forms,omitempty"`
	// Proposal is the proposal that added it, when the gate added it.
	Proposal string `yaml:"proposal,omitempty" json:"proposal,omitempty"`
}

// Registry is the registry file.
type Registry struct {
	Version int `yaml:"version"`
	// Imports are the modules the forms need; Opens the namespaces they are written in.
	Imports  []string  `yaml:"imports,omitempty"`
	Opens    []string  `yaml:"opens,omitempty"`
	Concepts []Concept `yaml:"concepts"`
}

// sentenceBreak finds the end of a sentence before the end of the text: a full stop, question or exclamation
// mark followed by a space and a capital letter.
var sentenceBreak = regexp.MustCompile(`[.!?]\s+\p{Lu}`)

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
	}
	return nil
}

// Check refuses an invalid registry: a concept without a name, home or one-sentence meaning, a name twice, an
// extends that names no registered concept, or a form that is not one lambda term.
func (r Registry) Check() error {
	if r.Version != RegistryVersion {
		return fmt.Errorf("registry version %d; this gate reads version %d", r.Version, RegistryVersion)
	}
	seen := map[string]bool{}
	for _, c := range r.Concepts {
		if err := c.check(); err != nil {
			return err
		}
		if seen[c.Name] {
			return fmt.Errorf("concept %s is registered twice", c.Name)
		}
		seen[c.Name] = true
	}
	for _, c := range r.Concepts {
		if c.Extends != "" && !seen[c.Extends] {
			return fmt.Errorf("concept %s extends %s, which is not registered", c.Name, c.Extends)
		}
	}
	return r.FormsModule().Check()
}

// Load reads and checks a registry file.
func Load(path string) (Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, err
	}
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return Registry{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := r.Check(); err != nil {
		return Registry{}, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
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

// FormsModule is the Lean module that the index compiles to match the registry's forms.
func (r Registry) FormsModule() declindex.FormsModule {
	fm := declindex.FormsModule{Imports: r.Imports, Opens: r.Opens}
	for _, c := range r.Concepts {
		fm.Concepts = append(fm.Concepts, declindex.ConceptForms{Concept: c.Name, Forms: c.Forms})
	}
	return fm
}

// ErrExists says that the registry already has a concept of the name.
var ErrExists = errors.New("already registered")

// Add appends a concept to the registry file. The existing text, comments included, stays as it is: the new
// entry is appended to the concepts list, which must be the file's last key.
func Add(path string, c Concept) error {
	r, err := Load(path)
	if err != nil {
		return err
	}
	if _, ok := r.Find(c.Name); ok {
		return fmt.Errorf("concept %s: %w", c.Name, ErrExists)
	}
	r.Concepts = append(r.Concepts, c)
	if err := r.Check(); err != nil {
		return err
	}
	entry, err := yaml.Marshal([]Concept{c})
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	var b strings.Builder
	b.WriteString(text)
	for _, line := range strings.Split(strings.TrimSuffix(string(entry), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if _, err := Load(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("the registry's concepts list is not its last key, so a concept cannot be appended: %w", err)
	}
	if got, _ := Load(tmp); len(got.Concepts) != len(r.Concepts) {
		_ = os.Remove(tmp)
		return errors.New("the registry's concepts list is not its last key, so a concept cannot be appended")
	}
	return os.Rename(tmp, path)
}
