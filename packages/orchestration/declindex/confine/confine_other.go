//go:build !linux

package confine

import "fmt"

func available() error { return fmt.Errorf("%w: Landlock and seccomp are Linux's", ErrUnavailable) }

func apply(Spec) error { return available() }
