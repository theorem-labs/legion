// Package graphcheck checks a landing batch's candidate tree against the declaration index of its base before a
// build: it refuses the name errors the build would certainly hit and flags the rest (README.md).
package graphcheck

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is graphcheck.yaml. Relative paths are relative to the file's directory.
type Config struct {
	// Index is the declindex configuration of the base's index.
	Index string `yaml:"index"`
	// Repository is the git repository that holds LAND_TREE and LAND_BASE.
	Repository string `yaml:"repository"`
	// Roots are the first components of the module names checked.
	Roots []string `yaml:"roots"`
	// BuildRoots are the root modules the build builds; their import closure is built.
	BuildRoots []string `yaml:"build_roots"`
	// RootNewModules says the build also builds every module new in the candidate.
	RootNewModules bool             `yaml:"root_new_modules"`
	Statements     StatementsConfig `yaml:"statements"`
	Links          LinksConfig      `yaml:"links"`
	Producers      ProducersConfig  `yaml:"producers"`
}

// StatementsConfig configures S2; it has no settings.
type StatementsConfig struct{}

// LinksConfig configures L1.
type LinksConfig struct {
	// LinkFiles and ExcludeFiles are repository-relative globs: L1 reads the files that match LinkFiles and not
	// ExcludeFiles.
	LinkFiles    []string `yaml:"link_files"`
	ExcludeFiles []string `yaml:"exclude_files"`
	// ContractName is an RE2 expression that the last component of a binder's compiled type head must match whole.
	ContractName string `yaml:"contract_name"`
	// OpenMarker followed by a reason of at least OpenMarkerMinReason characters exempts a binder.
	OpenMarker          string `yaml:"open_marker"`
	OpenMarkerMinReason int    `yaml:"open_marker_min_reason"`
	contract            *regexp.Regexp
}

// ProducersConfig configures P1.
type ProducersConfig struct {
	// InterfaceNames are RE2 expressions; a binder whose compiled type head's last component matches one whole
	// binds an interface.
	InterfaceNames []string `yaml:"interface_names"`
	// BinderFiles are repository-relative globs of the files whose binders P1 reads.
	BinderFiles []string `yaml:"binder_files"`
	interfaces  []*regexp.Regexp
}

// LoadConfig reads graphcheck.yaml strictly (an unknown key is an error naming it) and resolves its paths.
func LoadConfig(file string) (Config, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", file, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", file, err)
	}
	dir, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		return Config{}, err
	}
	for _, p := range []*string{&c.Index, &c.Repository} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return c, nil
}

func (c *Config) validate() error {
	switch {
	case c.Index == "":
		return fmt.Errorf("index is required: the declindex configuration of the base's index")
	case c.Repository == "":
		return fmt.Errorf("repository is required: the git repository that holds LAND_TREE and LAND_BASE")
	case len(c.Roots) == 0:
		return fmt.Errorf("roots are required: the first components of the module names checked")
	case len(c.BuildRoots) == 0:
		return fmt.Errorf("build_roots are required: the root modules the build builds")
	}
	for _, root := range c.BuildRoots {
		if !c.inRoots(root) {
			return fmt.Errorf("build_roots: %s is not a module of roots %v", root, c.Roots)
		}
	}
	l := &c.Links
	if err := checkGlobs("links.link_files", l.LinkFiles); err != nil {
		return err
	}
	if err := checkGlobs("links.exclude_files", l.ExcludeFiles); err != nil {
		return err
	}
	if len(l.LinkFiles) != 0 && l.ContractName == "" {
		return fmt.Errorf("links.contract_name is required when links.link_files is set")
	}
	if l.ContractName != "" {
		re, err := whole(l.ContractName)
		if err != nil {
			return fmt.Errorf("links.contract_name: %w", err)
		}
		l.contract = re
	}
	if l.OpenMarkerMinReason < 0 {
		return fmt.Errorf("links.open_marker_min_reason: %d is below 0", l.OpenMarkerMinReason)
	}
	p := &c.Producers
	if err := checkGlobs("producers.binder_files", p.BinderFiles); err != nil {
		return err
	}
	for i, name := range p.InterfaceNames {
		re, err := whole(name)
		if err != nil {
			return fmt.Errorf("producers.interface_names[%d]: %w", i, err)
		}
		p.interfaces = append(p.interfaces, re)
	}
	return nil
}

// inRoots reports whether a module's first component is one of the roots.
func (c Config) inRoots(module string) bool {
	first, _, _ := strings.Cut(module, ".")
	for _, root := range c.Roots {
		if first == root {
			return true
		}
	}
	return false
}

// IsLinkFile reports whether L1 reads the file: it matches link_files and not exclude_files.
func (l LinksConfig) IsLinkFile(file string) bool {
	return matchAny(l.LinkFiles, file) && !l.Excluded(file)
}

// Excluded reports whether the file matches exclude_files.
func (l LinksConfig) Excluded(file string) bool { return matchAny(l.ExcludeFiles, file) }

// IsContract reports whether the last component of a compiled type head matches contract_name whole.
func (l LinksConfig) IsContract(head string) bool {
	return l.contract != nil && l.contract.MatchString(head)
}

// IsBinderFile reports whether the file matches binder_files.
func (p ProducersConfig) IsBinderFile(file string) bool { return matchAny(p.BinderFiles, file) }

// IsInterface reports whether the last component of a compiled type head matches one of interface_names whole.
func (p ProducersConfig) IsInterface(head string) bool {
	for _, re := range p.interfaces {
		if re.MatchString(head) {
			return true
		}
	}
	return false
}

// whole compiles an RE2 expression that must match a whole string.
func whole(expr string) (*regexp.Regexp, error) {
	if _, err := regexp.Compile(expr); err != nil {
		return nil, err
	}
	return regexp.Compile(`^(?:` + expr + `)$`)
}

func checkGlobs(key string, globs []string) error {
	for i, glob := range globs {
		if glob == "" || strings.HasPrefix(glob, "/") {
			return fmt.Errorf("%s[%d]: %q is not a repository-relative glob", key, i, glob)
		}
		for _, component := range strings.Split(glob, "/") {
			if component == "**" {
				continue
			}
			if _, err := path.Match(component, ""); err != nil {
				return fmt.Errorf("%s[%d]: %q: %w", key, i, glob, err)
			}
		}
	}
	return nil
}

func matchAny(globs []string, file string) bool {
	parts := strings.Split(file, "/")
	for _, glob := range globs {
		if matchComponents(strings.Split(glob, "/"), parts) {
			return true
		}
	}
	return false
}

// matchComponents matches a glob's components against a path's: `**` matches zero or more whole components, any
// other component matches one component with path.Match (`*` stays within it).
func matchComponents(glob, parts []string) bool {
	if len(glob) == 0 {
		return len(parts) == 0
	}
	if glob[0] == "**" {
		for skip := 0; skip <= len(parts); skip++ {
			if matchComponents(glob[1:], parts[skip:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, err := path.Match(glob[0], parts[0])
	return err == nil && ok && matchComponents(glob[1:], parts[1:])
}
