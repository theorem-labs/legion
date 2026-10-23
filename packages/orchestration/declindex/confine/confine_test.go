package confine

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shell runs bash -c script confined by spec and returns its exit code and output. The tests need Landlock and
// seccomp (an unprivileged Linux process has both on a current kernel); without them they skip, unless
// DECLINDEX_REQUIRE_SANDBOX is set.
func shell(t *testing.T, spec Spec, script string) (int, string) {
	t.Helper()
	if err := Available(); err != nil {
		if os.Getenv("DECLINDEX_REQUIRE_SANDBOX") != "" {
			t.Fatalf("DECLINDEX_REQUIRE_SANDBOX is set and the sandbox is unavailable: %v", err)
		}
		t.Skipf("no sandbox: %v", err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	spec.Argv = []string{bash, "-c", script}
	cmd, err := Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, out.String()
}

func TestAConfinedProcessWritesOnlyBeneathItsWritableDirectory(t *testing.T) {
	work, outside := t.TempDir(), t.TempDir()
	spec := Spec{Write: []string{work}, Dir: work, Env: []string{"PATH=/usr/bin:/bin"}}
	if code, out := shell(t, spec, "echo x > "+filepath.Join(work, "in")); code != 0 {
		t.Fatalf("a write beneath the writable directory failed: %d %s", code, out)
	}
	if code, _ := shell(t, spec, "echo x > "+filepath.Join(outside, "out")); code == 0 {
		t.Fatal("a write outside the writable directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "out")); err == nil {
		t.Fatal("a file was written outside the writable directory")
	}
}

func TestAConfinedProcessReadsOnlyWhatItIsGiven(t *testing.T) {
	given, secret := t.TempDir(), t.TempDir()
	for _, d := range []string{given, secret} {
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := Spec{Read: []string{given}, Dir: given, Env: []string{"PATH=/usr/bin:/bin"}}
	if code, out := shell(t, spec, "read l < "+filepath.Join(given, "f")+" && echo $l"); code != 0 || !strings.Contains(out, "content") {
		t.Fatalf("a given file was not readable: %d %s", code, out)
	}
	if code, out := shell(t, spec, "read l < "+filepath.Join(secret, "f")+" && echo $l"); code == 0 || strings.Contains(out, "content") {
		t.Fatalf("a file outside the read set was readable: %d %s", code, out)
	}
	if code, _ := shell(t, spec, "read l < "+filepath.Join(os.Getenv("HOME"), ".bashrc")); code == 0 && os.Getenv("HOME") != "" {
		t.Fatal("the home directory was readable")
	}
}

func TestAConfinedProcessExecutesNothingButItsProgram(t *testing.T) {
	spec := Spec{Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}
	if code, out := shell(t, spec, "/bin/ls / > /dev/null"); code == 0 {
		t.Fatalf("another program ran: %s", out)
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
	spec := Spec{Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}
	if code, out := shell(t, spec, "exec 3<>/dev/tcp/127.0.0.1/"+port); code == 0 {
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
	spec := Spec{Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin", "ONLY=this"}}
	code, out := shell(t, spec, `echo "[$DECLINDEX_TEST_SECRET][$ONLY]"`)
	if code != 0 || !strings.Contains(out, "[][this]") {
		t.Fatalf("environment: %d %s", code, out)
	}
}
