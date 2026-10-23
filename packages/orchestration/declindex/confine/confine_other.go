//go:build !linux

package confine

import (
	"fmt"
	"os/exec"
)

func available() error { return fmt.Errorf("%w: Landlock and seccomp are Linux's", ErrUnavailable) }

func apply(Spec, int) error { return available() }

func start(*exec.Cmd) error { return available() }

// SetLandlockABIForTest has nothing to replace here: without Linux the sandbox is always unavailable.
func SetLandlockABIForTest(int, error) (restore func()) { return func() {} }
