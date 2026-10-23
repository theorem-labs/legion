package declindex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/orchestration/internal/testpg"
	"github.com/sjawhar/legion/orchestration/ledger"
)

func TestGlobRegexp(t *testing.T) {
	for glob, cases := range map[string]map[string]bool{
		"Src/**/Spec*.lean": {"Src/fs/a/Spec.lean": true, "Src/Spec.lean": true, "Src/fs/a/SpecX.lean": true,
			"Src/fs/a/Link.lean": false, "Other/a/Spec.lean": false},
		"**/*.lean": {"Golden.lean": true, "Sub/Wrap.lean": true, "Sub/Wrap.ilean": false},
	} {
		re, err := globRegexp(glob)
		if err != nil {
			t.Fatal(err)
		}
		for p, want := range cases {
			if re.MatchString(p) != want {
				t.Errorf("%s matches %s: %v, want %v", glob, p, !want, want)
			}
		}
	}
}

func TestLoadOwners(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	body := `{"format":"graph/owners/v1","owners":[{"id":"function:frame_at","role":"function","contract":"Sub"},` +
		`{"id":"struct:mount","role":"data_structure","contract":null}]}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := LoadOwners(p, "graph/owners/v1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o, Owners{"Sub": "function:frame_at"}) {
		t.Fatalf("owners %v", o)
	}
}

// After a landing the index registers new contracts and checks the rest; a change lock opened with the index's
// hashes records the ledger's versions; a landing that advances a contract to a hash the build lacks is reported.
func TestCheckLedgerRegistersChecksAndReportsDefects(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.Open(ctx, testpg.Database(t, "declindex_ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if _, err := l.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	s := refreshed(t, goldenTree(t))
	rule := ContractRule{Paths: []string{"**/*.lean"}, Kinds: []string{"structure", "def"}, Name: "^[A-Z]"}
	owners := Owners{"Sub": "function:frame_at"}
	r, err := s.CheckLedger(ctx, l, "r", rule, owners)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Registered, []string{"Gold2.FrameAt"}) || r.Contracts != 4 || len(r.Unowned) != 3 || len(r.Defects) != 0 {
		t.Fatalf("first check %+v", r)
	}
	found, _ := s.Resolve(ctx, "Gold2.FrameAt")
	hash := found[0].StmtHash
	c, err := l.Contract(ctx, "r", "Gold2.FrameAt")
	if err != nil || c.StatementHash != hash || c.Version != 1 || c.Owner != "function:frame_at" {
		t.Fatalf("registered contract %+v %v", c, err)
	}
	again, err := s.CheckLedger(ctx, l, "r", rule, owners)
	if err != nil || len(again.Registered) != 0 || again.Matching != 1 || len(again.Defects) != 0 {
		t.Fatalf("second check %+v %v", again, err)
	}
	m, err := l.OpenMigration(ctx, ledger.OpenRequest{Repo: "r", ID: "M-1", Owner: "function:frame_at",
		Writes: []string{"Sub/Wrap.lean"}, Reads: map[string]string{"Gold2.FrameAt": hash}})
	if err != nil {
		t.Fatal(err)
	}
	if v := m.ReadVersions["Gold2.FrameAt"]; v == nil || *v != 1 || m.Reads["Gold2.FrameAt"] != hash {
		t.Fatalf("the change lock did not record the index's hash and the ledger's version: %+v", m)
	}
	if _, err := l.Land(ctx, ledger.LandRequest{Repo: "r", Migration: "M-1", Generation: 1, Commit: "c2",
		ChangedPaths: []string{"Sub/Wrap.lean"}, Current: map[string]string{"Gold2.FrameAt": hash},
		ChangedContracts: map[string]string{"Gold2.FrameAt": "0000000000000000"},
		Head:             "c2", ReviewedHead: "c2"}); err != nil {
		t.Fatal(err)
	}
	after, err := s.CheckLedger(ctx, l, "r", rule, owners)
	if err != nil {
		t.Fatal(err)
	}
	want := []LedgerDefect{{Contract: "Gold2.FrameAt", At: "Sub/Wrap.lean:7", LedgerHash: "0000000000000000", Version: 2, IndexHash: hash}}
	if !reflect.DeepEqual(after.Defects, want) {
		t.Fatalf("defects %+v, want %+v", after.Defects, want)
	}
	if after.Gone != nil {
		t.Fatalf("gone %v before any contract left the index", after.Gone)
	}
	if _, err := l.RegisterContract(ctx, "r", "Gold.Removed", "function:frame_at", "1111111111111111", false); err != nil {
		t.Fatal(err)
	}
	gone, err := s.CheckLedger(ctx, l, "r", rule, owners)
	if err != nil || !reflect.DeepEqual(gone.Gone, []string{"Gold.Removed"}) {
		t.Fatalf("a ledger contract the index no longer declares: %+v %v", gone.Gone, err)
	}
}
