//go:build !linux

package confine

import (
	"fmt"
	"os/exec"
)

func available() error { return fmt.Errorf("%w: Landlock and seccomp are Linux's", ErrUnavailable) }

func apply(Spec, int) error { return available() }

func start(*exec.Cmd) error { return available() }
