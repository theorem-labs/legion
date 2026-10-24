//go:build !ledgertesthold

package main

// holdAfterCommit does nothing outside a build with the ledgertesthold tag; see hold_on.go.
func holdAfterCommit() {}
