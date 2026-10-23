package declindex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The exporter's Lean sources. It is built with the project's own toolchain, because a compiled module can be
// read only by the Lean that wrote it.
//
//go:embed lean/lakefile.toml lean/Main.lean lean/DeclExport/*.lean
var exporterSource embed.FS

// Lean is one Lean toolchain: the prefix that `lean --print-prefix` prints.
type Lean struct{ Prefix string }

func (l Lean) bin(name string) string { return filepath.Join(l.Prefix, "bin", name) }

// FindLean gives the toolchain that dir selects (its lean-toolchain file, through elan), or prefix when set.
func FindLean(ctx context.Context, dir, prefix string) (Lean, error) {
	if prefix != "" {
		if _, err := os.Stat(filepath.Join(prefix, "bin", "lean")); err != nil {
			return Lean{}, fmt.Errorf("toolchain %s has no bin/lean: %w", prefix, err)
		}
		return Lean{Prefix: prefix}, nil
	}
	cmd := exec.CommandContext(ctx, "lean", "--print-prefix")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Lean{}, fmt.Errorf("find the Lean toolchain of %s (lean --print-prefix): %w", dir, err)
	}
	return Lean{Prefix: strings.TrimSpace(string(out))}, nil
}

func (l Lean) env(searchPath []string) []string {
	env := []string{"LEAN_PATH=" + strings.Join(searchPath, string(os.PathListSeparator)),
		"PATH=" + filepath.Join(l.Prefix, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LEAN_PATH=") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	return env
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return nil
}

// BuildExporter builds the exporter with toolchain l below cacheDir, once for each version of its sources and
// each toolchain, and returns the binary. Lake compiles the modules to objects; the binary is linked against the
// toolchain's shared runtime (libleanshared), which needs no development libraries on the machine. cc is the C
// compiler that links (LEAN_CC, else the toolchain's own clang, else cc when empty).
func BuildExporter(ctx context.Context, l Lean, cc, cacheDir string) (string, error) {
	h := sha256.New()
	h.Write([]byte(l.Prefix))
	var files []string
	err := fs.WalkDir(exporterSource, "lean", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, f := range files {
		data, _ := exporterSource.ReadFile(f)
		h.Write([]byte(f))
		h.Write(data)
	}
	dir := filepath.Join(cacheDir, "exporter-"+hex.EncodeToString(h.Sum(nil))[:16])
	binPath := filepath.Join(dir, "declexport")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	for _, f := range files {
		data, _ := exporterSource.ReadFile(f)
		dst := filepath.Join(dir, strings.TrimPrefix(f, "lean/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
	}
	var modules, targets []string
	for _, f := range files {
		if strings.HasSuffix(f, ".lean") {
			m := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(f, "lean/"), ".lean"), "/", ".")
			modules = append(modules, m)
			targets = append(targets, "+"+m+":c.o.export")
		}
	}
	// Lake compiles with its own bundled compiler and flags unless a compiler is configured (a toolchain built from
	// source has none bundled).
	env := l.env(nil)
	if cc != "" {
		env = append(env, "LEAN_CC="+cc)
	}
	cc = linker(l, cc)
	if err := run(ctx, dir, env, l.bin("lake"), append([]string{"build"}, targets...)...); err != nil {
		return "", fmt.Errorf("build the exporter: %w", err)
	}
	libDir := filepath.Join(l.Prefix, "lib", "lean")
	args := []string{"-o", binPath + ".tmp"}
	for _, m := range modules {
		args = append(args, filepath.Join(dir, ".lake", "build", "ir", filepath.FromSlash(strings.ReplaceAll(m, ".", "/"))+".c.o.export"))
	}
	args = append(args, "-rdynamic", "-L"+libDir, "-Wl,-rpath,"+libDir)
	for _, lib := range []string{"leanshared", "Init_shared", "leanshared_1", "leanshared_2"} {
		if _, err := os.Stat(filepath.Join(libDir, "lib"+lib+".so")); err == nil {
			args = append(args, "-l"+lib)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cc), "ld.lld")); err == nil {
		args = append(args, "-fuse-ld=lld")
	}
	args = append(args, "-lm", "-ldl", "-pthread")
	if err := run(ctx, dir, l.env(nil), cc, args...); err != nil {
		return "", fmt.Errorf("link the exporter: %w", err)
	}
	if err := os.Rename(binPath+".tmp", binPath); err != nil {
		return "", err
	}
	return binPath, nil
}

// linker is the C compiler that links the exporter.
func linker(l Lean, cc string) string {
	if cc != "" {
		return cc
	}
	if v := os.Getenv("LEAN_CC"); v != "" {
		return v
	}
	if _, err := os.Stat(l.bin("clang")); err == nil {
		return l.bin("clang")
	}
	return "cc"
}

// CompileModule compiles the one file root/rel.lean to root/rel.olean and root/rel.ilean, against the modules
// of searchPath (and root), as `lean` does for one module of a build.
func CompileModule(ctx context.Context, l Lean, searchPath []string, root, rel string) error {
	base := filepath.Join(root, filepath.FromSlash(rel))
	return run(ctx, root, l.env(append([]string{root}, searchPath...)), l.bin("lean"), "-R", root,
		"-o", base+".olean", "-i", base+".ilean", base+".lean")
}

// ConstRef names a constant that stands for a concept: a form, or the concept's own declaration.
type ConstRef struct {
	Concept string `json:"concept"`
	Const   string `json:"const"`
}

// Manifest is one exporter run: the modules to read, extra modules to import (the forms), the project's roots
// (whose definitions are followed), the forms, and the concepts' own declarations (whose applications are uses).
type Manifest struct {
	Modules  []string   `json:"modules"`
	Imports  []string   `json:"imports"`
	Roots    []string   `json:"roots"`
	Forms    []ConstRef `json:"forms"`
	Concepts []ConstRef `json:"concepts"`
	Types    bool       `json:"types"`
	// Budget bounds the expression nodes one constant's walk visits; zero is DefaultBudget.
	Budget int `json:"budget"`
}

// DefaultBudget is the node budget of one constant's walk.
const DefaultBudget = 3000000

// complete gives every field a JSON value: the exporter reads no null.
func (m Manifest) complete() Manifest {
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	m.Modules, m.Imports, m.Roots = nonNil(m.Modules), nonNil(m.Imports), nonNil(m.Roots)
	if m.Forms == nil {
		m.Forms = []ConstRef{}
	}
	if m.Concepts == nil {
		m.Concepts = []ConstRef{}
	}
	if m.Budget == 0 {
		m.Budget = DefaultBudget
	}
	return m
}

// Fact is a registered concept that a statement states, and how: "direct", "via:<definition>" or "uses".
type Fact struct {
	Concept string `json:"c"`
	How     string `json:"how"`
}

// Constant is what the exporter read about one compiled constant.
type Constant struct {
	Name      string   `json:"n"`
	Module    string   `json:"m"`
	Kind      string   `json:"k"`
	Type      string   `json:"t"`
	Hash      string   `json:"h"`
	ValueHash string   `json:"vh"`
	Atoms     []string `json:"a"`
	Facts     []Fact   `json:"f"`
	Truncated bool     `json:"tr"`
}

// Exporter runs the built exporter over compiled modules.
type Exporter struct {
	Lean Lean
	Bin  string
	// SearchPath is LEAN_PATH: the project's library directory, its dependencies', and any extra directory
	// (forms, proposals).
	SearchPath []string
	// Jobs bounds the parallel exporter processes.
	Jobs int
	// ModulesPerProcess bounds one process's target modules; zero is all of a shard.
	ModulesPerProcess int
	Log               io.Writer
}

// ErrExporter wraps a failed exporter run: the question stays unanswered.
var ErrExporter = errors.New("exporter failed")

// Run exports the manifest's modules, in parallel processes of at most ModulesPerProcess modules each.
func (x *Exporter) Run(ctx context.Context, m Manifest) ([]Constant, error) {
	if len(m.Modules) == 0 {
		return nil, nil
	}
	per := x.ModulesPerProcess
	if per <= 0 {
		per = (len(m.Modules) + max(1, x.Jobs) - 1) / max(1, x.Jobs)
	}
	var chunks [][]string
	for i := 0; i < len(m.Modules); i += per {
		chunks = append(chunks, m.Modules[i:min(i+per, len(m.Modules))])
	}
	results := make([][]Constant, len(chunks))
	errs := make([]error, len(chunks))
	var mu sync.Mutex
	done := 0
	parallel(len(chunks), max(1, x.Jobs), func(i int) {
		mm := m
		mm.Modules = chunks[i]
		results[i], errs[i] = x.runOne(ctx, mm)
		mu.Lock()
		done++
		if errs[i] == nil && (done%10 == 0 || done == len(chunks)) {
			logf(x.Log, "exporter: %d of %d module groups done", done, len(chunks))
		}
		mu.Unlock()
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var out []Constant
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

func (x *Exporter) runOne(ctx context.Context, m Manifest) ([]Constant, error) {
	f, err := os.CreateTemp("", "declexport-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(m.complete()); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, x.Bin, f.Name())
	cmd.Env = x.Lean.env(x.SearchPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var out []Constant
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	var parseErr error
	for sc.Scan() {
		var c Constant
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil && parseErr == nil {
			parseErr = fmt.Errorf("exporter output: %w", err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil && parseErr == nil {
		parseErr = err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%w on %d modules (first %s): %v: %s", ErrExporter, len(m.Modules), m.Modules[0], err,
			strings.TrimSpace(stderr.String()))
	}
	if parseErr != nil {
		return nil, parseErr
	}
	if out == nil {
		out = []Constant{}
	}
	return out, nil
}
