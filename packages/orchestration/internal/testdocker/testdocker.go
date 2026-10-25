// Package testdocker gives a test the docker daemon and the image that the landing program's container tests run
// in. A test is skipped without docker, except on GitHub Actions, where docker is installed and a missing daemon is
// a failure.
package testdocker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Image has git 2.38 or later, GNU tar, coreutils and python3, which the program's steps and the fixtures use. It
// is pinned by digest.
const Image = "mirror.gcr.io/library/buildpack-deps@sha256:1314fd2a9c83b257b37132883b3c8e326d3909211bdcb419c9757399ebc34bf3"

// Require skips the test when docker cannot run a container, pulls Image, and returns the uid:gid that the
// containers run as: the test's own, so their writes in its temporary directories keep its owner.
func Require(t testing.TB) string {
	t.Helper()
	if output, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("docker is not available: %v: %s", err, output)
		}
		t.Skipf("docker is not available, so the container tests cannot run: %v", err)
	}
	if err := exec.Command("docker", "image", "inspect", Image).Run(); err != nil {
		if output, err := exec.Command("docker", "pull", "-q", Image).CombinedOutput(); err != nil {
			t.Fatalf("pull %s: %v: %s", Image, err, output)
		}
	}
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

// SSH is a container.ssh argv that stands in for ssh to the build VM: it drops ssh's options and the host, then
// runs the command in a shell on this host, as sshd runs it on the VM. Git's pushes take its words before the host
// as GIT_SSH_COMMAND, as they take ssh's, so a test drives the same path as production.
func SSH(t testing.TB) []string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "ssh")
	body := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -o|-p|-F|-i|-l) shift 2 ;; -*) shift ;; *) break ;; esac; done\nshift\nexec sh -c \"$*\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script, "build-vm"}
}
