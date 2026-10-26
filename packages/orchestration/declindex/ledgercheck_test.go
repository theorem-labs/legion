package declindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// The graph tool names a header helper's contract directory under each file that includes it: one owner, a list.
func TestLoadOwnersReadsAListOfContractDirectories(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "owners.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o, err := LoadOwners(write(`{"format":"f","owners":[`+
		`{"id":"function:helper@include/h.h","contract":["A/Helper","B/Helper"]},`+
		`{"id":"function:frame_at","contract":"Sub"},{"id":"struct:mount","contract":null}]}`), "f")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Owners{"A/Helper": "function:helper@include/h.h", "B/Helper": "function:helper@include/h.h", "Sub": "function:frame_at"}); !reflect.DeepEqual(o, want) {
		t.Fatalf("owners %v, want %v", o, want)
	}
	for name, body := range map[string]string{
		"a directory listed by two owners": `{"owners":[{"id":"function:a","contract":["D"]},{"id":"function:b","contract":"D"}]}`,
		"an empty directory in a list":     `{"owners":[{"id":"function:a","contract":["D",""]}]}`,
		"a number":                         `{"owners":[{"id":"function:a","contract":3}]}`,
	} {
		if _, err := LoadOwners(write(body), ""); err == nil {
			t.Errorf("%s was accepted", name)
		}
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

// Ledger contracts need not be selected by this index's contract rule. Every Lean-named contract is still checked
// against the declarations of that exact name, while metadata contracts with a colon are skipped.
func TestCheckLedgerChecksEveryLeanNamedLedgerContract(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.Open(ctx, testpg.Database(t, "declindex_all_ledger_contracts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if _, err := l.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	s := refreshed(t, goldenTree(t))
	matching, err := s.Resolve(ctx, "Gold2.FrameAt")
	if err != nil || len(matching) != 1 {
		t.Fatalf("matching declaration: %+v %v", matching, err)
	}
	wrong, err := s.Resolve(ctx, "Gold.Box")
	if err != nil || len(wrong) != 1 {
		t.Fatalf("wrong-hash declaration: %+v %v", wrong, err)
	}
	for _, c := range []struct {
		name, hash string
	}{
		{matching[0].Name, matching[0].StmtHash},
		{wrong[0].Name, "different"},
		{"spec:function:open_file", "metadata"},
	} {
		if _, err := l.RegisterContract(ctx, "r", c.name, "function:frame_at", c.hash, false); err != nil {
			t.Fatal(err)
		}
	}
	rule := ContractRule{Paths: []string{"NoContracts/**"}, Kinds: []string{"def", "structure"}}
	r, err := s.CheckLedger(ctx, l, "r", rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []LedgerDefect{{Contract: wrong[0].Name, At: wrong[0].At(), LedgerHash: "different", Version: 1, IndexHash: wrong[0].StmtHash}}
	if r.Matching != 1 || !reflect.DeepEqual(r.Defects, want) || r.Gone != nil || !r.Defective() {
		t.Fatalf("ledger report %+v, want one matching, one exact-name defect, no gone contracts", r)
	}
}

func TestCheckLedgerReportsAmbiguousExactLedgerName(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.Open(ctx, testpg.Database(t, "declindex_ambiguous_ledger_contract"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	if _, err := l.EnsureRepo(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	s := refreshed(t, goldenTree(t))
	found, err := s.Resolve(ctx, "Gold.Box")
	if err != nil || len(found) != 1 {
		t.Fatalf("declaration: %+v %v", found, err)
	}
	if _, err := l.RegisterContract(ctx, "r", found[0].Name, "function:frame_at", found[0].StmtHash, false); err != nil {
		t.Fatal(err)
	}
	execIndex(t, s, `INSERT INTO decls (name, short, module, kind, line, stmt_hash)
		VALUES ($1, $2, $3, $4, $5, $6)`, found[0].Name, "Box", "Other.Module", found[0].Kind, 1, found[0].StmtHash)
	rule := ContractRule{Paths: []string{"NoContracts/**"}, Kinds: []string{"def", "structure"}}
	r, err := s.CheckLedger(ctx, l, "r", rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Ambiguous, []string{found[0].Name}) || !r.Defective() {
		t.Fatalf("ledger report %+v, want the exact-name ambiguity", r)
	}
}

func TestOwnerMapsOfTheGraphTool(t *testing.T) {
	raw := os.Getenv("DECLINDEX_OWNER_MAPS")
	if raw == "" {
		t.Skip("DECLINDEX_OWNER_MAPS is unset")
	}
	nodeID := regexp.MustCompile(`^(?:(?:function|struct|global|percpu|subsystem):\S+|[a-z][a-z0-9-]*)$`)
	for _, path := range filepath.SplitList(raw) {
		if _, err := LoadOwners(path, "cgraph/owners/v1"); err != nil {
			t.Errorf("load %s: %v", path, err)
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		var ownerMap struct {
			Owners []struct {
				ID string `json:"id"`
			} `json:"owners"`
		}
		if err := json.Unmarshal(body, &ownerMap); err != nil {
			t.Errorf("parse %s: %v", path, err)
			continue
		}
		for _, owner := range ownerMap.Owners {
			if !nodeID.MatchString(owner.ID) {
				t.Errorf("%s has owner id %q outside contract 1's patterns", path, owner.ID)
			}
		}
	}
}
