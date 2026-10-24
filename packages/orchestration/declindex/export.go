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
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/sjawhar/legion/orchestration/declindex/confine"
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

// env is the whole environment of a toolchain process: the toolchain first on a system PATH, its root
// (LEAN_SYSROOT, so that nothing runs `lean` to find it), LEAN_PATH, and HOME and TMPDIR at home. Nothing else of the caller's environment (credentials, tokens) reaches Lean, and a
// module that a proposal compiles sees only its scratch directory as home.
func (l Lean) env(searchPath []string, home string) []string {
	env := []string{
		"LEAN_SYSROOT=" + l.Prefix,
		"LEAN_PATH=" + strings.Join(searchPath, string(os.PathListSeparator)),
		"PATH=" + strings.Join([]string{filepath.Join(l.Prefix, "bin"), "/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)),
		"HOME=" + home,
		"TMPDIR=" + home,
	}
	if v := os.Getenv("LANG"); v != "" {
		env = append(env, "LANG="+v)
	}
	return env
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	InGroup(cmd)
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
	// source has none bundled): the configured cc, else the caller's LEAN_CC, which l.env does not pass on.
	if cc == "" {
		cc = os.Getenv("LEAN_CC")
	}
	env := l.env(nil, os.TempDir())
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
	if err := run(ctx, dir, l.env(nil, os.TempDir()), cc, args...); err != nil {
		return "", fmt.Errorf("link the exporter: %w", err)
	}
	if err := os.Rename(binPath+".tmp", binPath); err != nil {
		return "", err
	}
	return binPath, nil
}

// linker is the C compiler that links the exporter: cc (the configured compiler or LEAN_CC), else the toolchain's
// own clang, else cc on PATH.
func linker(l Lean, cc string) string {
	if cc != "" {
		return cc
	}
	if _, err := os.Stat(l.bin("clang")); err == nil {
		return l.bin("clang")
	}
	return "cc"
}

// CompileModule compiles the one file root/rel.lean to root/rel.olean and root/rel.ilean, against the modules
// of searchPath (and root), as `lean` does for one module of a build. Lean runs in root with root as its home and
// the minimal environment of Lean.env.
func CompileModule(ctx context.Context, l Lean, searchPath []string, root, rel string) error {
	base := filepath.Join(root, filepath.FromSlash(rel))
	return run(ctx, root, l.env(append([]string{root}, searchPath...), root), l.bin("lean"), "-R", root,
		"-o", base+".olean", "-i", base+".ilean", base+".lean")
}

// NewScratch makes a fresh scratch directory for one confined run below base (the system's temporary directory
// when empty): this user's, mode 0700, and empty.
func NewScratch(base string) (string, error) { return confine.NewScratch(base) }

// confined runs a toolchain program in the sandbox of package confine: it reads the toolchain, the search path,
// read and its scratch directory, writes only the scratch directory (which must be fresh, and into which the
// inputs are copied), starts no other process and opens no socket. It fails when the sandbox is unavailable,
// and then nothing runs.
func (l Lean) confined(ctx context.Context, argv []string, scratch string, inputs []confine.Input, searchPath, read []string) (*confine.Cmd, error) {
	reads := append(append([]string{l.Prefix}, searchPath...), read...)
	return confine.Command(ctx, confine.Spec{Argv: argv, Env: l.env(searchPath, scratch), Scratch: scratch, Inputs: inputs, Read: reads})
}

// ErrSourceFailed says that Lean ran on an untrusted source and reported errors in it: the source's failure, not
// the sandbox's or the toolchain's.
var ErrSourceFailed = errors.New("the source does not compile")

// CompileConfined compiles source as the module rel in the sandbox, for source that untrusted text wrote. scratch
// must be fresh (NewScratch): the source is copied into it, and Lean writes its outputs there and nowhere else. An
// error that Lean reports in the source wraps ErrSourceFailed; any other failure (the sandbox, a signal) does not.
func CompileConfined(ctx context.Context, l Lean, searchPath []string, scratch, rel string, source []byte) error {
	base := filepath.Join(scratch, filepath.FromSlash(rel))
	argv := []string{l.bin("lean"), "-R", scratch, "-o", base + ".olean", "-i", base + ".ilean", base + ".lean"}
	cmd, err := l.confined(ctx, argv, scratch, []confine.Input{{Name: rel + ".lean", Data: source}},
		append([]string{scratch}, searchPath...), nil)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 1 && ctx.Err() == nil:
		// Lean exits 1 when the file has errors; the launcher's own failures exit 126.
		return fmt.Errorf("%w: lean %s: %v\n%s", ErrSourceFailed, base+".lean", err, out.String())
	default:
		return fmt.Errorf("lean %s: %w\n%s", base+".lean", err, out.String())
	}
}

// CheckSandbox reports whether toolchain l runs in the sandbox here: the sandbox is available, and a confined
// `lean --version` runs.
func CheckSandbox(ctx context.Context, l Lean) error {
	dir, err := NewScratch("")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cmd, err := l.confined(ctx, []string{l.bin("lean"), "--version"}, dir, nil, nil, nil)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: a confined lean --version failed: %v: %s", confine.ErrUnavailable, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ErrRefusedSource says that a proposal's source holds more than declarations.
var ErrRefusedSource = errors.New("the proposal is not declarations only")

// CheckSource parses source with Lean's own parser, in the sandbox, and refuses it unless it holds only
// declarations (see lean/DeclExport/Check.lean). searchPath holds the modules it imports; scratch must be fresh.
func (x *Exporter) CheckSource(ctx context.Context, source []byte, searchPath []string, scratch string) error {
	file := filepath.Join(scratch, "Proposal.lean")
	cmd, err := x.Lean.confined(ctx, []string{x.Bin, "--check", file}, scratch,
		[]confine.Input{{Name: "Proposal.lean", Data: source}}, searchPath, nil)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return fmt.Errorf("%w: %s", ErrRefusedSource, strings.TrimSpace(out.String()))
	default:
		return fmt.Errorf("%w: the source check failed: %v: %s", ErrExporter, err, strings.TrimSpace(out.String()))
	}
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

// nonNil gives an empty slice for nil, so that JSON writes [] and not null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// complete gives every field a JSON value: the exporter reads no null.
func (m Manifest) complete() Manifest {
	m.Modules, m.Imports, m.Roots = nonNil(m.Modules), nonNil(m.Imports), nonNil(m.Roots)
	m.Forms, m.Concepts = nonNil(m.Forms), nonNil(m.Concepts)
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
	// Confined runs the exporter in the sandbox, for modules that untrusted source wrote; CompiledOptions then
	// compiles the forms module in the sandbox too. It is the one switch of a confined export.
	Confined bool
	// ScratchDir holds each run's fresh scratch directory; empty is the system's temporary directory.
	ScratchDir string
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
	chunks := slices.Collect(slices.Chunk(m.Modules, per))
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

// started is a program the exporter reads: exec.Cmd, or confine.Cmd, whose Start also lets its one exec through.
type started interface {
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
}

func (x *Exporter) runOne(ctx context.Context, m Manifest) ([]Constant, error) {
	manifest, err := json.Marshal(m.complete())
	if err != nil {
		return nil, err
	}
	tmp, err := NewScratch(x.ScratchDir)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "manifest.json")
	var stderr bytes.Buffer
	var cmd started
	if x.Confined {
		c, err := x.Lean.confined(ctx, []string{x.Bin, path}, tmp, []confine.Input{{Name: "manifest.json", Data: manifest}},
			x.SearchPath, nil)
		if err != nil {
			return nil, err
		}
		c.Stderr = &stderr
		cmd = c
	} else {
		if err := os.WriteFile(path, manifest, 0o600); err != nil {
			return nil, err
		}
		c := exec.CommandContext(ctx, x.Bin, path)
		c.Env = x.Lean.env(x.SearchPath, tmp)
		InGroup(c)
		c.Stderr = &stderr
		cmd = c
	}
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
