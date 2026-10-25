//go:build linux

package confine

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireSandbox(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") != "" {
			t.Fatalf("DECLINDEX_REQUIRE_SANDBOX is set and the sandbox is unavailable: %v", err)
		}
		t.Skipf("no sandbox: %v", err)
	}
}

func bashPath(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	bash, err = filepath.EvalSymlinks(bash)
	if err != nil {
		t.Fatal(err)
	}
	return bash
}

// scratch is a fresh scratch directory for one confined run.
func scratch(t *testing.T) string {
	t.Helper()
	d, err := NewScratch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// shell runs bash -c script confined by spec and returns its exit code and output. The tests need Landlock (ABI
// 3 or newer) and seccomp; without them they skip, unless DECLINDEX_REQUIRE_SANDBOX is set.
func shell(t *testing.T, spec Spec, script string) (int, string) {
	t.Helper()
	requireSandbox(t)
	spec.Argv = []string{bashPath(t), "-c", script}
	if spec.Env == nil {
		spec.Env = []string{"PATH=/usr/bin:/bin"}
	}
	cmd, err := Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func TestAConfinedProcessWritesOnlyBeneathItsScratch(t *testing.T) {
	outside := t.TempDir()
	work := scratch(t)
	if code, out := shell(t, Spec{Scratch: work}, "echo x > "+filepath.Join(work, "in")); code != 0 {
		t.Fatalf("a write in the scratch directory failed: %d %s", code, out)
	}
	work = scratch(t)
	if code, _ := shell(t, Spec{Scratch: work}, "echo x > "+filepath.Join(outside, "out")); code == 0 {
		t.Fatal("a write outside the scratch directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "out")); err == nil {
		t.Fatal("a file was written outside the scratch directory")
	}
}

func TestAConfinedProcessReadsOnlyWhatItIsGiven(t *testing.T) {
	given, secret := t.TempDir(), t.TempDir()
	for _, d := range []string{given, secret} {
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := func() Spec { return Spec{Read: []string{given}, Scratch: scratch(t)} }
	if code, out := shell(t, spec(), "read l < "+filepath.Join(given, "f")+" && echo $l"); code != 0 || !strings.Contains(out, "content") {
		t.Fatalf("a given file was not readable: %d %s", code, out)
	}
	if code, out := shell(t, spec(), "read l < "+filepath.Join(secret, "f")+" && echo $l"); code == 0 || strings.Contains(out, "content") {
		t.Fatalf("a file outside the read set was readable: %d %s", code, out)
	}
	if home := os.Getenv("HOME"); home != "" {
		if code, _ := shell(t, spec(), "read l < "+filepath.Join(home, ".bashrc")); code == 0 {
			t.Fatal("the home directory was readable")
		}
	}
}

// Inputs are copied into the scratch directory, never linked: the program reads them, and writing them leaves
// their sources as they were.
func TestInputsAreCopiesInTheScratch(t *testing.T) {
	work := scratch(t)
	spec := Spec{Scratch: work, Inputs: []Input{{Name: "sub/in.txt", Data: []byte("given\n")}}}
	code, out := shell(t, spec, "read l < sub/in.txt && echo $l && echo changed > sub/in.txt")
	if code != 0 || !strings.Contains(out, "given") {
		t.Fatalf("the input was not readable: %d %s", code, out)
	}
}

// A process the sandbox starts is its only process: it cannot fork, cannot execute another program or itself
// again, and cannot run a program through the dynamic loader it is allowed to load.
func TestAConfinedProcessStartsNoOtherProcess(t *testing.T) {
	bash := bashPath(t)
	interp, err := interpreter(bash)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bash)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"fork":       "( echo x > %s ); true",
		"exec other": "exec /bin/sh -c 'echo x > %s'",
		"exec self":  "exec " + bash + " -c 'echo x > %s'",
	}
	if interp != "" {
		cases["loader"] = "exec " + interp + " ./sh -c 'echo x > %s'"
	}
	for name, script := range cases {
		work := scratch(t)
		target := filepath.Join(work, "written")
		spec := Spec{Scratch: work, Inputs: []Input{{Name: "sh", Data: data, Mode: 0o700}}}
		code, out := shell(t, spec, strings.ReplaceAll(script, "%s", target))
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s: another process ran (exit %d): %s", name, code, out)
		}
	}
}

func TestAConfinedProcessOpensNoSocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	if code, out := shell(t, Spec{Scratch: scratch(t)}, "exec 3<>/dev/tcp/127.0.0.1/"+port); code == 0 {
		t.Fatalf("a TCP connection opened: %s", out)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection from the confined process")
	default:
	}
}

func TestTheEnvironmentIsExactlyTheSpecs(t *testing.T) {
	t.Setenv("DECLINDEX_TEST_SECRET", "leaked")
	spec := Spec{Scratch: scratch(t), Env: []string{"PATH=/usr/bin:/bin", "ONLY=this"}}
	code, out := shell(t, spec, `echo "[$DECLINDEX_TEST_SECRET][$ONLY]"`)
	if code != 0 || !strings.Contains(out, "[][this]") {
		t.Fatalf("environment: %d %s", code, out)
	}
}

// The scratch directory must be fresh: this user's, closed to others, and empty. An entry placed in it before
// the run (a hard link to a file outside) would give the program that file.
func TestCommandRefusesAScratchThatIsNotFresh(t *testing.T) {
	requireSandbox(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := scratch(t)
	if err := os.Link(outside, filepath.Join(linked, "alias")); err != nil {
		t.Fatal(err)
	}
	open := scratch(t)
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(scratch(t), link); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"an entry": linked, "open to others": open, "a symlink": link,
		"missing": filepath.Join(t.TempDir(), "none"), "none": ""} {
		if _, err := Command(context.Background(), Spec{Argv: []string{bashPath(t), "-c", "true"}, Scratch: dir}); err == nil {
			t.Errorf("a scratch directory with %s was accepted", name)
		}
	}
	for _, name := range []string{"../x", "/abs", "a/../../x", ""} {
		_, err := Command(context.Background(), Spec{Argv: []string{bashPath(t), "-c", "true"}, Scratch: scratch(t),
			Inputs: []Input{{Name: name, Data: []byte("x")}}})
		if err == nil {
			t.Errorf("the input name %q was accepted", name)
		}
	}
}

// Landlock before ABI 3 does not govern truncation: a confined process could empty any file it may read. Such a
// kernel has no sandbox.
func TestALandlockWithoutTruncationIsNoSandbox(t *testing.T) {
	saved := landlockVersion
	t.Cleanup(func() { landlockVersion = saved })
	for _, abi := range []int{1, 2} {
		landlockVersion = func() (int, error) { return abi, nil }
		if err := Available(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("ABI %d: Available() = %v", abi, err)
		}
		if _, err := Command(context.Background(), Spec{Argv: []string{"/bin/true"}, Scratch: scratch(t)}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("ABI %d: Command() = %v", abi, err)
		}
	}
}
