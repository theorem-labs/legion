package graphcheck_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/orchestration/graphcheck"
	"github.com/sjawhar/legion/orchestration/graphcheck/graphchecktest"
	"github.com/sjawhar/legion/orchestration/landing"
	"github.com/sjawhar/legion/orchestration/landing/blame"
)

func links(t *testing.T, f *graphchecktest.Fixture, tree string) []landing.Finding {
	t.Helper()
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Links(context.Background(), c, idx, f.Cfg)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	return hits
}

func producers(t *testing.T, f *graphchecktest.Fixture, tree string) []landing.Finding {
	t.Helper()
	c, idx := open(t, f, tree)
	hits, err := graphcheck.Producers(context.Background(), c, idx, f.Cfg)
	if err != nil {
		t.Fatalf("Producers: %v", err)
	}
	return hits
}

// linkFile is a link file of directory Lib/Drv whose theorem keeps a hypothesis of type head; above is the line
// above the theorem and trailing what follows the theorem on its line.
func linkFile(head, above, trailing string) string {
	return "import Lib.Spec.LinkSpec\n\nnamespace Lib.Drv\n\n" + above + "\ntheorem drv_ok (h : " + head + ") : True := trivial" +
		trailing + "\n\nend Lib.Drv\n"
}

func TestL1FlagsAHypothesisALinkKeeps(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/LinkDrv.lean", "", linkFile("Lib.Spec.SIZE_OK", "", ""))
	hits := links(t, f, tree)
	h, ok := find(hits, landing.VerdictFlag, blame.Changed, "Lib.Drv.drv_ok", "Lib/Drv/LinkDrv.lean", "L1")
	if !ok || h.Line != 6 || h.Col != 16 {
		t.Fatalf("no L1 flag at Lib/Drv/LinkDrv.lean:6:16 in %v", hits)
	}
	if !strings.Contains(h.Reason, "Lib/Spec/LinkSpec.lean") {
		t.Fatalf("reason %q names no other file", h.Reason)
	}
}

// A head written short, through an open namespace, resolves to the same record.
func TestL1ResolvesAHeadThroughAnOpenNamespace(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/LinkDrv.lean", "", linkFile("SIZE_OK", "open Lib.Spec", ""))
	if _, ok := find(links(t, f, tree), landing.VerdictFlag, blame.Changed, "Lib.Drv.drv_ok", "Lib/Drv/LinkDrv.lean", "L1"); !ok {
		t.Fatal("no L1 flag for a head written through open")
	}
}

// The fixture's open_marker_min_reason is 20: a reason of 19 characters does not exempt the binder, one of 20 does.
func TestL1OpenMarkerNeedsTheMinimumReason(t *testing.T) {
	for _, c := range []struct {
		name, above, trailing string
		flagged               bool
	}{
		{"19 above", "-- open ask: " + strings.Repeat("r", 19), "", true},
		{"20 above", "-- open ask: " + strings.Repeat("r", 20), "", false},
		{"20 on the binder's line", "", " -- open ask:   " + strings.Repeat("é", 20), false},
		{"19 on the binder's line", "", " -- open ask: " + strings.Repeat("é", 19), true},
		{"20 in the docstring", "/-- The record is kept.\n  open ask: " + strings.Repeat("d", 10) + "\n" + strings.Repeat("d", 9) + " -/", "", false},
		{"no marker", "-- " + strings.Repeat("r", 40), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := graphchecktest.New(t)
			tree := f.Edit(t, "Lib/Drv/LinkDrv.lean", "", linkFile("Lib.Spec.SIZE_OK", c.above, c.trailing))
			_, ok := find(links(t, f, tree), landing.VerdictFlag, blame.Changed, "Lib.Drv.drv_ok", "Lib/Drv/LinkDrv.lean", "L1")
			if ok != c.flagged {
				t.Fatalf("flagged = %v, want %v", ok, c.flagged)
			}
		})
	}
}

func TestL1IgnoresAnExcludedPath(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Skip/LinkDrv.lean", "", linkFile("Lib.Spec.SIZE_OK", "", ""))
	if hits := ruleHits(links(t, f, tree), "L1"); len(hits) != 0 {
		t.Fatalf("L1 findings %v in an excluded file", hits)
	}
}

func TestL1IgnoresASameDirectoryLink(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Spec/LinkMore.lean", "", linkFile("Lib.Spec.SIZE_OK", "", ""))
	if hits := ruleHits(links(t, f, tree), "L1"); len(hits) != 0 {
		t.Fatalf("L1 findings %v for a record linked in the file's own directory", hits)
	}
}

func TestL1IgnoresAFileThatIsNotALink(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/Driver.lean", "", linkFile("Lib.Spec.SIZE_OK", "", ""))
	if hits := ruleHits(links(t, f, tree), "L1"); len(hits) != 0 {
		t.Fatalf("L1 findings %v in a file link_files does not match", hits)
	}
}

func TestL1IgnoresAHeadThatIsNotAContractName(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Spec/LinkSpec.lean": {"theorem size_ok : SIZE_OK := ⟨rfl⟩", "theorem size_ok : SIZE_OK := ⟨rfl⟩\n\ntheorem port_ok : Lib.Iface.Port := ⟨1⟩"},
		"Lib/Drv/LinkDrv.lean":   {"", linkFile("Lib.Iface.Port", "", "")},
	})
	if hits := ruleHits(links(t, f, tree), "L1"); len(hits) != 0 {
		t.Fatalf("L1 findings %v for a head contract_name does not match", hits)
	}
}

// A record that a changed link file of another directory concludes counts as the base's do.
func TestL1ReadsTheCandidatesChangedLinkFiles(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.EditMany(t, map[string][2]string{
		"Lib/Net/LinkNet.lean": {"", "namespace Lib.Net\n\nstructure NET_OK : Prop where\n  up : True\n\ntheorem net_ok : NET_OK := ⟨trivial⟩\n\nend Lib.Net\n"},
		"Lib/Drv/LinkDrv.lean": {"", "import Lib.Net.LinkNet\n\nnamespace Lib.Drv\n\ntheorem drv_ok (h : Lib.Net.NET_OK) : True := trivial\n\nend Lib.Drv\n"},
	})
	h, ok := find(links(t, f, tree), landing.VerdictFlag, blame.Changed, "Lib.Drv.drv_ok", "Lib/Drv/LinkDrv.lean", "L1")
	if !ok || !strings.Contains(h.Reason, "Lib/Net/LinkNet.lean") {
		t.Fatalf("no L1 flag naming Lib/Net/LinkNet.lean: %+v", h)
	}
}

func TestL1IgnoresAPrivateTheorem(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/LinkDrv.lean", "", strings.Replace(linkFile("Lib.Spec.SIZE_OK", "", ""), "theorem drv_ok", "private theorem drv_ok", 1))
	if hits := ruleHits(links(t, f, tree), "L1"); len(hits) != 0 {
		t.Fatalf("L1 findings %v for a private theorem", hits)
	}
}

func TestP1FlagsAnInterfaceWithNoProducer(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/Use.lean", "", "import Lib.Iface\n\nnamespace Lib.Drv\n\ntheorem uses_port (p : Lib.Iface.Port) : True := trivial\n\nend Lib.Drv\n")
	hits := producers(t, f, tree)
	h, ok := find(hits, landing.VerdictFlag, blame.Changed, "Lib.Drv.uses_port", "Lib/Drv/Use.lean", "P1")
	if !ok || h.Line != 5 || h.Col != 19 {
		t.Fatalf("no P1 flag at Lib/Drv/Use.lean:5:19 in %v", hits)
	}
	if !strings.Contains(h.Reason, "Lib.Iface.Port") || !strings.Contains(h.Reason, "Lib/Iface.lean") {
		t.Fatalf("reason %q", h.Reason)
	}
}

// An interface that an instance, a definition or a use of its constructor produces needs nothing more.
func TestP1IgnoresAProducedInterface(t *testing.T) {
	use := "import Lib.Iface\n\nnamespace Lib.Drv\n\ntheorem uses_port (p : Lib.Iface.Port) : True := trivial\n\nend Lib.Drv\n"
	for name, edits := range map[string]map[string][2]string{
		// The base's instance of Sized moves to another file under a name: sized_ok keeps a producer.
		"instance": {
			"Lib/Iface.lean": {"instance : Sized Nat := ⟨fun n => n⟩\n", ""},
			"Lib/Make2.lean": {"", "import Lib.Iface\n\ninstance sizedNat : Lib.Iface.Sized Nat := ⟨fun n => n + 0⟩\n"},
		},
		"definition": {
			"Lib/Drv/Use.lean": {"", use},
			"Lib/Make2.lean":   {"", "import Lib.Iface\n\ndef somePort (n : Nat) : Lib.Iface.Port := ⟨n⟩\n"},
		},
		"constructor": {
			"Lib/Drv/Use.lean": {"", use},
			"Lib/Make2.lean":   {"", "import Lib.Iface\n\ndef ports := [Lib.Iface.Port.mk 1]\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := graphchecktest.New(t)
			if hits := ruleHits(producers(t, f, f.EditMany(t, edits)), "P1"); len(hits) != 0 {
				t.Fatalf("P1 findings %v for an interface %s produces", hits, name)
			}
		})
	}
}

func TestP1IgnoresAFileOutsideTheBinderFiles(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Other/Use.lean", "", "import Lib.Iface\n\ntheorem uses_port (p : Lib.Iface.Port) : True := trivial\n")
	if hits := ruleHits(producers(t, f, tree), "P1"); len(hits) != 0 {
		t.Fatalf("P1 findings %v outside binder_files", hits)
	}
}

// Lib/Drv/Base.lean's had_port has no producer at the base already: editing its file reports nothing.
func TestP1IgnoresAFindingPresentAtTheBase(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Drv/Base.lean", "end Lib.Drv", "theorem extra : True := trivial\n\nend Lib.Drv")
	if hits := ruleHits(producers(t, f, tree), "P1"); len(hits) != 0 {
		t.Fatalf("P1 findings %v present at the base", hits)
	}
}

// Removing the base's only producer of a class makes the unchanged binders of it findings, named by the producer.
func TestP1FlagsTheBindersOfAProducerTheCandidateRemoved(t *testing.T) {
	f := graphchecktest.New(t)
	tree := f.Edit(t, "Lib/Iface.lean", "instance : Sized Nat := ⟨fun n => n⟩\n", "")
	hits := ruleHits(producers(t, f, tree), "P1")
	if len(hits) != 1 || hits[0].Path != "Lib/Drv/Base.lean" || hits[0].Line != 7 || hits[0].Verdict != landing.VerdictFlag ||
		hits[0].Declaration != "Lib.Iface.instSizedNat" || !strings.Contains(hits[0].Reason, "Lib.Drv.sized_ok") {
		t.Fatalf("P1 findings %v, want one at Lib/Drv/Base.lean:7 named by the removed instance", hits)
	}
}
