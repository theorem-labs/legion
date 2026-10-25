package main

import (
	"path/filepath"
	"testing"
)

// A configured member carries the upstream commit of its ready record, which the runner verifies its head against
// before the ledger checks it. The verification itself is covered through the real git adapter in the landing
// package.
func TestReadConfigCarriesAMemberUpstreamCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "land.json")
	write(t, path, `{"members": [{"migration": "sync", "generation": 1, "head": "head-sync", "upstream": "upstream-1"},
		{"migration": "ordinary", "generation": 1, "head": "head-ordinary"}]}`)
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig() error = %v", err)
	}
	if len(cfg.Members) != 2 || cfg.Members[0].Upstream != "upstream-1" || cfg.Members[1].Upstream != "" {
		t.Fatalf("the configured members are %+v", cfg.Members)
	}
}
