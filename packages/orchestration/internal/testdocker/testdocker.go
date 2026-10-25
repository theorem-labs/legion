// Package testdocker gives a test the docker daemon and the image that the landing program's container tests run
// in. A test is skipped without docker, except on GitHub Actions, where docker is installed and a missing daemon is
// a failure.
package testdocker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Image is the image every container test runs in. It is built on each host from recipe, the first time a test
// asks for it, and its tag is the recipe's digest, so a changed recipe is a new image and no host keeps running an
// old one.
var Image = "legion-testdocker:" + digest(recipe)

// recipe is the project's landing image without the landing program's own files: the same Ubuntu 26.04 base,
// pinned by digest, and the same packages at the build machine's versions, so that a test runs what production
// runs. A Lean toolchain is mounted, never installed, as in production.
//
// The base is Ubuntu 26.04 because the project's Lean toolchains are built there: their libleanshared.so needs
// GLIBC_2.43 (`objdump -T`), and the Debian bookworm image this replaced has 2.36, so no `lean` started in it.
//
// The packages, as the landing image lists them:
//   - git, python3, util-linux, coreutils, findutils, tar: the landing program's steps and the fixtures;
//   - libuv1t64: `ldd` on the toolchain's `lean` and `lake` lists libuv.so.1, which nothing else installs;
//   - libssl3t64, libgmp10, libstdc++6, libgcc-s1, zlib1g, libzstd1, libc6: the rest of that `ldd` list, pinned so
//     that the toolchain loads the build machine's own versions;
//   - clang-21 (as `cc`), llvm-21, lld-21, libgmp-dev, libssl-dev, libuv1-dev: the C compiler. A toolchain built from
//     source bundles none, so Lake compiles C with `cc`, the index's exporter links with it, and a precompiled
//     library links with `cc -shared -fuse-ld=lld ... libgmp.so -luv libssl.so libcrypto.so`.
//
// One package is the tests' own, not the landing image's: curl, with which the landing package's network tests show
// that a container with a network reaches a server over HTTP and one without does not. Without curl the second half
// would pass for want of a client.
//
// A version the archive has dropped fails the build and names the package: move the pin to the build machine's
// new version, as the landing image is rebuilt after the machine is upgraded.
const recipe = `FROM mirror.gcr.io/library/ubuntu:26.04@sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944
RUN export DEBIAN_FRONTEND=noninteractive \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      git=1:2.53.0-1ubuntu1.1 python3=3.14.3-0ubuntu2 util-linux=2.41.3-3ubuntu2.2 \
      coreutils=9.5-1ubuntu2+0.0.0~ubuntu25 findutils=4.10.0-3build2 tar=1.35+dfsg-4ubuntu0.4 \
      libuv1t64=1.51.0-2ubuntu1 libssl3t64=3.5.5-1ubuntu3.7 libgmp10=2:6.3.0+dfsg-5ubuntu2 \
      libstdc++6=16-20260322-1ubuntu1 libgcc-s1=16-20260322-1ubuntu1 zlib1g=1:1.3.dfsg+really1.3.1-1ubuntu3.2 \
      libzstd1=1.5.7+dfsg-3 libc6=2.43-2ubuntu2.4 \
      clang-21=1:21.1.8-6ubuntu1 llvm-21=1:21.1.8-6ubuntu1 lld-21=1:21.1.8-6ubuntu1 \
      libgmp-dev=2:6.3.0+dfsg-5ubuntu2 libssl-dev=3.5.5-1ubuntu3.7 libuv1-dev=1.51.0-2ubuntu1 \
      curl=8.18.0-1ubuntu2.8 \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/lib/llvm-21/bin/clang /usr/local/bin/cc
`

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// Require skips the test when docker cannot run a container, builds Image when this host has none, and returns
// the uid:gid that the containers run as: the test's own, so their writes in its temporary directories keep its
// owner.
func Require(t testing.TB) string {
	t.Helper()
	if output, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("docker is not available: %v: %s", err, output)
		}
		t.Skipf("docker is not available, so the container tests cannot run: %v", err)
	}
	if err := exec.Command("docker", "image", "inspect", Image).Run(); err != nil {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(recipe), 0o644); err != nil {
			t.Fatal(err)
		}
		// Two test binaries may build at once; docker tags the same image, so the second build only retags.
		if output, err := exec.Command("docker", "build", "-q", "-t", Image, dir).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", Image, err, output)
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
