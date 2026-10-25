package graphcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// lakeSetup is the part of a module's setup file (`<build>/ir/<Module>.setup.json`) that Lake hands Lean: the
// package the module belongs to, the options of its package and library, and the shared libraries Lean loads as
// plugins (the toolchain's, the dependencies', and the precompiled modules' of its import closure).
type lakeSetup struct {
	Package string                     `json:"package"`
	Options map[string]json.RawMessage `json:"options"`
	Plugins []struct {
		Path string `json:"path"`
	} `json:"plugins"`
}

// lakeState is one leanArgs call's view of the base build's setup files: each file read once, and each changed
// module's plugins computed once.
type lakeState struct {
	// mu guards every map below: the compiles that ask for their arguments run in parallel.
	mu      sync.Mutex
	c       *Candidate
	dir     string
	setups  map[string]*lakeSetup // module -> its base setup, nil when it has none
	plugins map[string][]string   // changed module -> the plugins of its candidate imports
	pending map[string]bool       // changed modules whose plugins are being computed
	stale   map[string]string     // shared library path -> the changed precompiled module it belongs to
}

// leanArgs gives the arguments Lake gave a module of the base build, for the scratch compile of a candidate module:
// `-D<name>=<value>` for each option of its package and library, and `--plugin=<path>` for each shared library
// Lake loads into its compile, sorted and deduplicated. They come from the base build's setup files.
//
// A modified module takes its own setup's options; an added module those of a direct import of its library (the
// same first name component), else of any direct import with a setup. Its plugins are its own setup's, and for
// each direct import, that import's plugins (its base setup's, or, for a changed import, what leanArgs computes
// for it) and the import's own precompiled shared library. A changed module that is precompiled has a stale
// shared library in the base build, which the check cannot rebuild: a compile that would load it is unable.
// A tree that Lake did not build has no setup directory and gives no arguments.
func (c *Candidate) leanArgs(m *Module) ([]string, error) {
	dir := filepath.Join(filepath.Dir(filepath.Dir(c.icfg.Lib)), "ir")
	if info, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		return nil, nil
	} else if err != nil {
		return nil, unable("the base build's setup directory %s cannot be read: %v", dir, err)
	}
	c.lakeOnce.Do(func() {
		s := &lakeState{c: c, dir: dir, setups: map[string]*lakeSetup{}, plugins: map[string][]string{},
			pending: map[string]bool{}, stale: map[string]string{}}
		c.lakeErr = s.findStale()
		c.lake = s
	})
	if c.lakeErr != nil {
		return nil, c.lakeErr
	}
	s := c.lake
	s.mu.Lock()
	defer s.mu.Unlock()
	own, err := s.setup(m.Module)
	if err != nil {
		return nil, err
	}
	options := own
	if options == nil {
		if options, err = s.inheritedOptions(m); err != nil {
			return nil, err
		}
	}
	plugins, err := s.importPlugins(m.Module, m.Imports)
	if err != nil {
		return nil, err
	}
	if own != nil {
		for _, p := range own.Plugins {
			plugins = append(plugins, p.Path)
		}
	}
	var args []string
	if options != nil {
		for name, raw := range options.Options {
			value, err := optionValue(raw)
			if err != nil {
				return nil, unable("the base build's setup of %s has option %s: %v", m.Module, name, err)
			}
			args = append(args, "-D"+name+"="+value)
		}
	}
	for _, p := range plugins {
		if module, ok := s.stale[p]; ok {
			return nil, staleLibrary(module, m.Module)
		}
		args = append(args, "--plugin="+p)
	}
	sort.Strings(args)
	return slices.Compact(args), nil
}

func staleLibrary(module, importer string) error {
	return unable("%s is a precompiled module that the candidate changes, and compiling %s loads its shared library; "+
		"the check cannot rebuild a precompiled module's shared library", module, importer)
}

// findStale records the base shared library of every changed module that has one.
func (s *lakeState) findStale() error {
	for name := range s.c.modules {
		setup, err := s.setup(name)
		if err != nil {
			return err
		}
		if setup == nil {
			continue
		}
		if lib, ok := s.sharedLibrary(name, setup.Package); ok {
			s.stale[lib] = name
		}
	}
	return nil
}

// inheritedOptions is the setup whose options an added module takes: that of a direct import of its library, else
// of any direct import, in name order.
func (s *lakeState) inheritedOptions(m *Module) (*lakeSetup, error) {
	first, _, _ := strings.Cut(m.Module, ".")
	var fallback *lakeSetup
	for _, imp := range slices.Sorted(slices.Values(m.Imports)) {
		setup, err := s.setup(imp)
		if err != nil {
			return nil, err
		}
		if setup == nil {
			continue
		}
		if impFirst, _, _ := strings.Cut(imp, "."); impFirst == first {
			return setup, nil
		}
		if fallback == nil {
			fallback = setup
		}
	}
	return fallback, nil
}

// importPlugins gives the plugins a module's direct imports bring in the candidate.
func (s *lakeState) importPlugins(module string, imports []string) ([]string, error) {
	var out []string
	for _, imp := range imports {
		setup, err := s.setup(imp)
		if err != nil {
			return nil, err
		}
		if changed := s.c.modules[imp]; changed != nil {
			if setup != nil {
				if _, ok := s.sharedLibrary(imp, setup.Package); ok {
					return nil, staleLibrary(imp, module)
				}
			}
			plugins, err := s.changedPlugins(changed)
			if err != nil {
				return nil, err
			}
			out = append(out, plugins...)
			continue
		}
		if setup == nil {
			continue
		}
		for _, p := range setup.Plugins {
			out = append(out, p.Path)
		}
		if lib, ok := s.sharedLibrary(imp, setup.Package); ok {
			out = append(out, lib)
		}
	}
	return out, nil
}

// changedPlugins gives a changed module's plugins: its own setup's and those its candidate imports bring.
func (s *lakeState) changedPlugins(m *Module) ([]string, error) {
	if plugins, ok := s.plugins[m.Module]; ok {
		return plugins, nil
	}
	if s.pending[m.Module] {
		return nil, unable("the candidate's imports of %s form a cycle", m.Module)
	}
	s.pending[m.Module] = true
	defer delete(s.pending, m.Module)
	plugins, err := s.importPlugins(m.Module, m.Imports)
	if err != nil {
		return nil, err
	}
	setup, err := s.setup(m.Module)
	if err != nil {
		return nil, err
	}
	if setup != nil {
		for _, p := range setup.Plugins {
			plugins = append(plugins, p.Path)
		}
	}
	s.plugins[m.Module] = plugins
	return plugins, nil
}

// setup reads a module's base setup file, or gives nil when the base build has none.
func (s *lakeState) setup(module string) (*lakeSetup, error) {
	if setup, ok := s.setups[module]; ok {
		return setup, nil
	}
	path := filepath.Join(s.dir, filepath.FromSlash(strings.ReplaceAll(module, ".", "/"))+".setup.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.setups[module] = nil
		return nil, nil
	}
	if err != nil {
		return nil, unable("the base build's setup of %s cannot be read: %v", module, err)
	}
	var setup lakeSetup
	if err := json.Unmarshal(raw, &setup); err != nil {
		return nil, unable("the base build's setup of %s (%s) is not Lake's: %v", module, path, err)
	}
	s.setups[module] = &setup
	return &setup, nil
}

// sharedLibrary is the module's precompiled shared library in the base build's library directory, when it has one.
// Lake names it by Lean's module initialization stem: `<package>_<Module>`, mangled.
func (s *lakeState) sharedLibrary(module, pkg string) (string, bool) {
	path := filepath.Join(s.c.icfg.Lib, initializationStem(module, pkg)+".so")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, true
	}
	return "", false
}

// optionValue writes a setup option's JSON value as `-D` takes it: a Boolean or a number as written, a string
// unquoted.
func optionValue(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	text := string(raw)
	if text == "true" || text == "false" {
		return text, nil
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return text, nil
	}
	return "", fmt.Errorf("value %s is not a Boolean, a number or a string", text)
}

// initializationStem is Lean's mkModuleInitializationStem (Lean/Compiler/NameMangling.lean): the mangled package
// name, `_`, and the mangled module name.
func initializationStem(module, pkg string) string {
	var b strings.Builder
	if pkg != "" {
		b.WriteString(mangleString(pkg))
		b.WriteByte('_')
	}
	prev := ""
	for i, part := range strings.Split(module, ".") {
		m := mangleString(part)
		switch {
		case i == 0 && needsDisambiguation(m):
			b.WriteString("00")
		case i > 0 && (strings.HasSuffix(prev, "_") || needsDisambiguation(m)):
			b.WriteString("_00")
		case i > 0:
			b.WriteByte('_')
		}
		b.WriteString(m)
		prev = part
	}
	return b.String()
}

// mangleString is Lean's String.Internal.mangle.
func mangleString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteString("__")
		case r < 0x100:
			fmt.Fprintf(&b, "_x%02x", r)
		case r < 0x10000:
			fmt.Fprintf(&b, "_u%04x", r)
		default:
			fmt.Fprintf(&b, "_U%08x", r)
		}
	}
	return b.String()
}

// needsDisambiguation is Lean's checkDisambiguation: whether a mangled component could be read as an escape.
func needsDisambiguation(m string) bool {
	for i := range len(m) {
		switch c := m[i]; {
		case c == '_':
			continue
		case c == 'x':
			return lowerHex(m[i+1:], 2)
		case c == 'u':
			return lowerHex(m[i+1:], 4)
		case c == 'U':
			return lowerHex(m[i+1:], 8)
		default:
			return c >= '0' && c <= '9'
		}
	}
	return true
}

func lowerHex(s string, n int) bool {
	if len(s) < n {
		return false
	}
	for _, c := range []byte(s[:n]) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
