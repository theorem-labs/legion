package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ownerpkg "github.com/sjawhar/legion/orchestration/owner"
)

// TestOwnerHelperProcess is the owner command as a process of its own, for the tests that run two commands of one
// owner at once: it runs the newline-separated arguments of OWNER_HELPER_ARGS and exits with the command's code.
func TestOwnerHelperProcess(t *testing.T) {
	spec := os.Getenv("OWNER_HELPER_ARGS")
	if spec == "" {
		return
	}
	os.Exit(run(context.Background(), strings.Split(spec, "\n"), os.Getenv, os.Stdout, os.Stderr))
}

// gatedCommand is one owner command running as its own process, whose first read of record from HEAD returns only
// once the test releases it: git ran, and the command has the bytes, but cannot act on them yet.
type gatedCommand struct {
	t       *testing.T
	gate    string
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	done    chan error
}

// gitGate puts a git on PATH that pauses the first `git show HEAD:<record>` of a command whose OWNER_TEST_GATE names
// an armed gate, after the real git ran: as a scheduler can pause a command between its read and its write.
func (f *commandFixture) gitGate(record string) {
	f.t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		f.t.Fatal(err)
	}
	dir := filepath.Join(f.root, "gitgate")
	script := `#!/bin/sh
if [ -n "$OWNER_TEST_GATE" ] && [ "$1" = show ] && [ "$2" = 'HEAD:` + record + `' ] && [ $# -eq 2 ] &&
	mv "$OWNER_TEST_GATE/armed" "$OWNER_TEST_GATE/claimed" 2>/dev/null; then
	'` + realGit + `' "$@" > "$OWNER_TEST_GATE/out"
	status=$?
	: > "$OWNER_TEST_GATE/arrived"
	while [ ! -e "$OWNER_TEST_GATE/release" ]; do sleep 0.02; done
	cat "$OWNER_TEST_GATE/out"
	exit $status
fi
exec '` + realGit + `' "$@"
`
	f.writeFile(filepath.Join(dir, "git"), script)
	if err := os.Chmod(filepath.Join(dir, "git"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// startGated starts owner's command as a process of its own, with an armed gate.
func (o ownerRun) startGated(name string, args ...string) *gatedCommand {
	o.f.t.Helper()
	gate := filepath.Join(o.f.root, "gates", name)
	o.f.writeFile(filepath.Join(gate, "armed"), "")
	command := exec.Command(os.Args[0], "-test.run=^TestOwnerHelperProcess$")
	command.Env = append(os.Environ(),
		"OWNER_HELPER_ARGS="+strings.Join(append([]string{"--deploy", o.f.deploy, "--owner", o.owner}, args...), "\n"),
		"OWNER_TEST_GATE="+gate, "ENVOY_URL="+o.f.env["ENVOY_URL"], "ENVOY_TOKEN="+o.f.env["ENVOY_TOKEN"], "HOME="+o.f.env["HOME"])
	g := &gatedCommand{t: o.f.t, gate: gate, command: command, done: make(chan error, 1)}
	command.Stdout, command.Stderr = &g.stdout, &g.stderr
	if err := command.Start(); err != nil {
		o.f.t.Fatal(err)
	}
	o.f.t.Cleanup(func() {
		_ = command.Process.Kill()
		<-g.done
	})
	go func() { g.done <- command.Wait() }()
	return g
}

// arrived waits until the command holds its first read of the record.
func (g *gatedCommand) arrived() {
	g.t.Helper()
	waitUntil(g.t, 60*time.Second, "a command to read the record", g.hasArrived)
}

func (g *gatedCommand) hasArrived() bool {
	_, err := os.Stat(filepath.Join(g.gate, "arrived"))
	return err == nil
}

// readsOrWaitsFor waits until the command either holds its first read of the record or waits for the worktree's
// commit lock, whose file it then has open: a command that reads the record only under that lock cannot read it while
// another command holds it.
func (g *gatedCommand) readsOrWaitsFor(lock string) {
	g.t.Helper()
	want, err := filepath.EvalSymlinks(lock)
	if err != nil {
		want = lock
	}
	fds := filepath.Join("/proc", strconv.Itoa(g.command.Process.Pid), "fd")
	waitUntil(g.t, 60*time.Second, "a command to read the record or wait for "+lock, func() bool {
		if g.hasArrived() {
			return true
		}
		entries, _ := os.ReadDir(fds)
		for _, entry := range entries {
			if target, err := os.Readlink(filepath.Join(fds, entry.Name())); err == nil && target == want {
				return true
			}
		}
		return false
	})
}

// commitLock is the worktree's commit lock file, owner.flock in its git directory.
func (f *commandFixture) commitLock(owner string) string {
	f.t.Helper()
	path := f.git(f.wt(owner), "rev-parse", "--git-path", "owner.flock")
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.wt(owner), path)
	}
	return path
}

// release lets the command act on its read, and waits for it to exit.
func (g *gatedCommand) release() (string, int) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.gate, "release"), nil, 0o600); err != nil {
		g.t.Fatal(err)
	}
	select {
	case err := <-g.done:
		g.done <- err
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			g.t.Fatal(err)
		}
		return g.stdout.String() + g.stderr.String(), code
	case <-time.After(120 * time.Second):
		g.t.Fatalf("the command did not exit: %s%s", g.stdout.String(), g.stderr.String())
		return "", -1
	}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Two `owner need` commands of one owner with different drafts, the second paused after its first read of the need
// record until the target has answered the first: the second is a new revision with a new open item, never the
// first's revision with the first's done item (which lost the changed need).
func TestConcurrentNeedsOfOneOwnerAreTwoRevisions(t *testing.T) {
	f := newCommandFixture(t)
	edge := ownerpkg.EdgeID(caller, state)
	needPath := ownerpkg.NeedRecord("skeleton", caller, state)
	f.as(caller).ok("worktree")
	f.as(state).ok("worktree")
	f.gitGate(needPath)
	second := strings.Replace(needDraft, "while it reads it", "while it holds the state lock", 1)

	first := f.as(caller).startGated("first", "--json", "need", state, f.write("need-first.md", needDraft))
	first.arrived()
	later := f.as(caller).startGated("second", "--json", "need", state, f.write("need-second.md", second))
	later.readsOrWaitsFor(f.commitLock(caller))
	if out, code := first.release(); code != 0 {
		t.Fatalf("the first need exited %d: %s", code, out)
	}
	later.arrived()
	f.as(state).ok("offer", f.write("offer.md", offerDraft), "--answers", "need:"+edge+"@r1")
	if item := f.item("need:" + edge + "@r1"); item.State != "done" {
		t.Fatalf("the first need = %+v, want done", item)
	}

	out, code := later.release()
	t.Logf("the second need exited %d: %s", code, out)
	var front ownerpkg.NeedFront
	if err := front.Unmarshal(frontMatter(t, f.show(caller, needPath))); err != nil {
		t.Fatal(err)
	}
	switch code {
	case 0:
		if front.Revision != 2 || !strings.Contains(string(f.show(caller, needPath)), "holds the state lock") {
			t.Fatalf("after the second need (%s) the record is r%d:\n%s", out, front.Revision, f.show(caller, needPath))
		}
		if item := f.item("need:" + edge + "@r2"); item.State != "open" || item.Owner != state {
			t.Fatalf("the second need's item = %+v, want open for %s", item, state)
		}
	default:
		// A refusal must leave the first need as it was.
		if front.Revision != 1 || strings.Contains(string(f.show(caller, needPath)), "holds the state lock") {
			t.Fatalf("the refused second need (exit %d: %s) changed the record to r%d", code, out, front.Revision)
		}
	}
}

// Two `owner offer` commands of one owner that answer two needs, the second paused after its first read of the spec:
// they are two spec revisions, and each source's offer names the revision that answers its need.
func TestConcurrentOffersOfOneOwnerAreTwoRevisions(t *testing.T) {
	f := newCommandFixture(t)
	fromCaller := "need:" + ownerpkg.EdgeID(caller, state) + "@r1"
	fromSecond := "need:" + ownerpkg.EdgeID(second, state) + "@r1"
	f.as(caller).ok("need", state, f.write("need-caller.md", needDraft))
	f.as(second).ok("need", state, f.write("need-second.md", needDraft))
	f.as(state).ok("worktree")
	specPath := ownerpkg.SpecRecord("skeleton", state)
	f.gitGate(specPath)
	changed := strings.Replace(offerDraft, "the last value that a writer stored", "the value that the last writer stored", 1)

	first := f.as(state).startGated("first", "--json", "offer", f.write("offer-first.md", offerDraft), "--answers", fromCaller)
	first.arrived()
	later := f.as(state).startGated("second", "--json", "offer", f.write("offer-second.md", changed), "--answers", fromSecond)
	later.readsOrWaitsFor(f.commitLock(state))
	if out, code := first.release(); code != 0 {
		t.Fatalf("the first offer exited %d: %s", code, out)
	}
	later.arrived()
	if out, code := later.release(); code != 0 {
		t.Fatalf("the second offer exited %d: %s", code, out)
	}

	if count := f.git(f.root, "--git-dir="+f.bare, "rev-list", "--count", "owner/"+ownerpkg.Key(state), "--", specPath); count != "2" {
		t.Fatalf("spec commits = %s, want two revisions", count)
	}
	for _, offer := range []struct{ item, source string }{
		{"offer:" + ownerpkg.EdgeID(caller, state) + "@r1", caller},
		{"offer:" + ownerpkg.EdgeID(second, state) + "@r2", second},
	} {
		if item := f.item(offer.item); item.State != "open" || item.Owner != offer.source {
			t.Errorf("%s = %+v, want open for %s", offer.item, item, offer.source)
		}
	}
	front, _, err := ownerpkg.ParseSpec(f.show(state, specPath))
	if err != nil {
		t.Fatal(err)
	}
	secondNeed := "need:" + ownerpkg.EdgeID(second, state) + "@" + ownerpkg.ContentHash(f.show(second, ownerpkg.NeedRecord("skeleton", second, state)))
	if front.Revision != 2 || len(front.Answers) != 1 || front.Answers[0] != secondNeed {
		t.Fatalf("the spec after both offers = %+v, want r2 answering %s", front, secondNeed)
	}
}

// A refused notice is retried and journaled after the step has let go of the owner's commit lock: a second command of
// the same owner commits and returns while the listener still refuses the first command's notice.
func TestASecondCommandDoesNotWaitOnTheFirstCommandsRefusedNotice(t *testing.T) {
	f := newCommandFixture(t)
	f.as(caller).ok("worktree")
	firstDraft, secondDraft := f.write("need-state.md", needDraft), f.write("need-cluster.md", needDraft)
	f.mu.Lock()
	f.refuseTopic = "notifications.example.node.struct:state.needs"
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	first := make(chan int, 1)
	go func() {
		_, _, code := f.as(caller).runContext(ctx, "need", state, firstDraft)
		first <- code
	}()
	waitUntil(t, 60*time.Second, "the listener to refuse the first need's notice", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.refused) > 0
	})
	type exit struct {
		code   int
		stderr string
	}
	second := make(chan exit, 1)
	go func() {
		_, stderr, code := f.as(caller).call("need", "subsystem:cluster", secondDraft)
		second <- exit{code, stderr}
	}()
	select {
	case got := <-second:
		if got.code != 0 {
			t.Fatalf("the second need exited %d: %s", got.code, got.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second need waited for the first need's refused notice")
	}
	select {
	case code := <-first:
		t.Fatalf("the first need exited %d before its notice's retries ended", code)
	default:
	}
	if item := f.item("need:" + ownerpkg.EdgeID(caller, "subsystem:cluster") + "@r1"); item.State != "open" {
		t.Fatalf("the second need's item = %+v", item)
	}
	cancel()
	<-first
}
