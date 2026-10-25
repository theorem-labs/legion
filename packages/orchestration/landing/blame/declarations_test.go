package blame

import "testing"

func TestScanDeclarationsQualifiesScopesAndModifiers(t *testing.T) {
	source := `namespace Alpha
namespace Beta
@[tag] noncomputable unsafe partial def annotated := 0
protected def guarded := 1
private def hidden := 2
section Local
def scoped := 3
end Local
end Beta
namespace A.B
def dotted := 4
end A.B
def _root_.rooted := 5
end Alpha
`
	got := ScanDeclarations("Declarations.lean", source)
	want := []Declaration{
		{File: "Declarations.lean", Name: "Alpha.Beta.annotated", Text: "@[tag] noncomputable unsafe partial def annotated := 0"},
		{File: "Declarations.lean", Name: "Alpha.Beta.guarded", Text: "protected def guarded := 1", Protected: true},
		{File: "Declarations.lean", Name: "Alpha.Beta.hidden", Text: "private def hidden := 2", Private: true},
		{File: "Declarations.lean", Name: "Alpha.Beta.scoped", Text: "def scoped := 3"},
		{File: "Declarations.lean", Name: "Alpha.A.B.dotted", Text: "def dotted := 4"},
		{File: "Declarations.lean", Name: "rooted", Text: "def _root_.rooted := 5"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() returned %d declarations: %#v, want %d", len(got), got, len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("declaration %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestScanDeclarationsDoesNotScopeCommentsOrStrings(t *testing.T) {
	source := `namespace Alpha
-- namespace Beta
/- namespace Gamma
  /- end Alpha -/
-/
def first := "end Alpha"
def second := 0
end Alpha
`
	got := ScanDeclarations("Comments.lean", source)
	want := []Declaration{
		{File: "Comments.lean", Name: "Alpha.first", Text: `def first := "end Alpha"`},
		{File: "Comments.lean", Name: "Alpha.second", Text: "def second := 0"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() returned %d declarations: %#v, want %d", len(got), got, len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("declaration %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

// A universe-polymorphic name `S.{u}` declares S, and an instance with a
// priority still declares its name.
func TestScanDeclarationsNamesUniversesAndPriorities(t *testing.T) {
	source := `namespace A
structure S.{u} (α : Sort u) where
  x : α
def T.{u, v} (α : Sort u) (β : Sort v) : Prop := True
instance (priority := low) feedR_drop : Inhabited Nat := ⟨0⟩
end A
`
	got := ScanDeclarations("Universe.lean", source)
	var names []string
	for _, declaration := range got {
		names = append(names, declaration.Name)
	}
	if want := []string{"A.S", "A.T", "A.feedR_drop"}; !sameStrings(names, want) {
		t.Fatalf("ScanDeclarations() names = %q, want %q", names, want)
	}
	if found := DeclarationsFor("A.S", got); len(found) != 1 || found[0].Name != "A.S" {
		t.Fatalf("DeclarationsFor(A.S) = %+v, want the universe-polymorphic structure", found)
	}
}

func declarationNames(declarations []Declaration) []string {
	names := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		names = append(names, declaration.Name)
	}
	return names
}

// Each source holds a construct that must not change the namespace that
// `after` is declared in.
func TestScanDeclarationsKeepsScopeAcrossConstructs(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{"char literal quote", "namespace Alpha\ndef q := '\"' /- \"\nnamespace Hidden\n-/\ndef after := 0\nend Alpha\n", []string{"Alpha.q", "Alpha.after"}},
		{"escaped char literal", "namespace Alpha\ndef q := '\\'' -- \"\ndef after := 0\nend Alpha\n", []string{"Alpha.q", "Alpha.after"}},
		{"multi-line string", "namespace Alpha\ndef s := \"first\nend Alpha\nstill string\"\ndef after := 0\nend Alpha\n", []string{"Alpha.s", "Alpha.after"}},
		{"attributed section", "namespace Alpha\n@[expose] public section\ndef inSection := 0\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.inSection", "Alpha.after"}},
		{"class inductive", "namespace Alpha\nclass inductive Good : Prop\n  | intro\nclass abbrev Both := Good\ndef after := 0\nend Alpha\n", []string{"Alpha.Good", "Alpha.Both", "Alpha.after"}},
		{"mutual", "namespace Alpha\nmutual\n  def even : Nat → Bool\n    | 0 => true\n    | n + 1 => odd n\n  def odd : Nat → Bool\n    | 0 => false\n    | n + 1 => even n\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.even", "Alpha.odd", "Alpha.after"}},
		{"anonymous section", "namespace Alpha\nsection\nvariable (n : Nat)\ndef inSection := n\nend\ndef after := 0\nend Alpha\n", []string{"Alpha.inSection", "Alpha.after"}},
		{"deriving", "namespace Alpha\nstructure Point where\n  x : Nat\nderiving Repr\ndef after := 0\nend Alpha\n", []string{"Alpha.Point", "Alpha.after"}},
		{"attribute line", "namespace Alpha\n@[simp]\ntheorem lemma_one : True := trivial\ndef after := 0\nend Alpha\n", []string{"Alpha.lemma_one", "Alpha.after"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := declarationNames(ScanDeclarations("Construct.lean", test.source)); !sameStrings(got, test.want) {
				t.Fatalf("ScanDeclarations() names = %q, want %q", got, test.want)
			}
		})
	}
}

// A modifier written on its own line belongs to the declaration below it.
func TestScanDeclarationsCarriesModifierLines(t *testing.T) {
	source := "namespace Alpha\nprivate\ndef hidden := 0\n@[simp] protected\ntheorem guarded : True := trivial\ndef open_ := 0\nend Alpha\n"
	got := ScanDeclarations("Modifiers.lean", source)
	if names := declarationNames(got); !sameStrings(names, []string{"Alpha.hidden", "Alpha.guarded", "Alpha.open_"}) {
		t.Fatalf("ScanDeclarations() names = %q", names)
	}
	if !got[0].Private || got[0].Protected {
		t.Errorf("hidden = %+v, want private only", got[0])
	}
	if got[1].Private || !got[1].Protected {
		t.Errorf("guarded = %+v, want protected only", got[1])
	}
	if got[2].Private || got[2].Protected {
		t.Errorf("open_ = %+v, want neither modifier", got[2])
	}
}

// A declaration's Text is its whole signature: the header and every
// continuation line up to the body, joined with single spaces. Comments and
// blank lines inside the signature are not part of it, and a match arm of a
// definition starts its body.
func TestScanDeclarationsJoinsSignatureLines(t *testing.T) {
	source := "theorem T (n : Nat)\n    (h : n = 0) -- why\n\n    : True := by\n  trivial\n" +
		"def f : Nat → Nat\n  | 0 => 1\n  | n + 1 => n\n" +
		"structure Point (α : Type)\n    (β : Type) where\n  x : α\n" +
		"inductive Tree\n  | leaf\n  | node (l r : Tree)\n" +
		"theorem open_ (n : Nat)\n    (h : n = 0)\n"
	got := ScanDeclarations("Signatures.lean", source)
	want := map[string]string{
		"T":     "theorem T (n : Nat) (h : n = 0) : True := by",
		"f":     "def f : Nat → Nat",
		"Point": "structure Point (α : Type) (β : Type) where",
		"Tree":  "inductive Tree | leaf | node (l r : Tree)",
		"open_": "theorem open_ (n : Nat) (h : n = 0)",
	}
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() = %+v, want %d declarations", got, len(want))
	}
	for _, declaration := range got {
		if declaration.Text != want[declaration.Name] {
			t.Errorf("%s Text = %q, want %q", declaration.Name, declaration.Text, want[declaration.Name])
		}
	}
}

// A signature ends only where its body starts: a `:=`, a closing `where`, or
// a definition's first match arm, at bracket depth 0 across all its lines.
// Lean continues a term on an unindented line, so such a line stays in the
// signature, while a syntax command such as macro_rules ends the
// declaration. Brackets inside string and character literals do not count.
func TestScanDeclarationsEndsSignatureOnlyAtBody(t *testing.T) {
	for _, test := range []struct {
		name, source string
		text         map[string]string
		// owners is each line's owner: "sig Name" for a signature line,
		// "body Name" for a body line and "" for a line of no declaration.
		owners []string
	}{{
		name: "binder open across an unindented line",
		source: "private theorem Table.noLink_of [Group G] {A :\n" +
			"Table.Args G} (O : Table.Open A)\n" +
			"    (F : Table.Fits A) : Table.NoLink A where\n" +
			"  read := O.read\n",
		text:   map[string]string{"Table.noLink_of": "private theorem Table.noLink_of [Group G] {A : Table.Args G} (O : Table.Open A) (F : Table.Fits A) : Table.NoLink A where"},
		owners: []string{"sig Table.noLink_of", "sig Table.noLink_of", "sig Table.noLink_of", "body Table.noLink_of", ""},
	}, {
		name:   "operand after an operator on an unindented line",
		source: "theorem iter_lt (n : Nat) :\n    ⊢ P n -∗\nctx n -∗\n        R n := by\n  trivial\n",
		text:   map[string]string{"iter_lt": "theorem iter_lt (n : Nat) : ⊢ P n -∗ ctx n -∗ R n := by"},
		owners: []string{"sig iter_lt", "sig iter_lt", "sig iter_lt", "sig iter_lt", "body iter_lt", ""},
	}, {
		name:   "list element on an unindented line",
		source: "theorem t : P [a,\nBitVec.reduceNeg,\n    b] := by\n  trivial\n",
		text:   map[string]string{"t": "theorem t : P [a, BitVec.reduceNeg, b] := by"},
		owners: []string{"sig t", "sig t", "sig t", "body t", ""},
	}, {
		name:   "default value inside a binder that spans lines",
		source: "def g (cfg : Nat\n    := 0)\n    (h : cfg = 0) : True := trivial\n",
		text:   map[string]string{"g": "def g (cfg : Nat := 0) (h : cfg = 0) : True := trivial"},
		owners: []string{"sig g", "sig g", "sig g", ""},
	}, {
		name:   "brackets inside literals",
		source: "def s (x : String := \"(\") (c : Char := '[') : Nat :=\n  0\n",
		text:   map[string]string{"s": "def s (x : String := \"(\") (c : Char := '[') : Nat :="},
		owners: []string{"sig s", "body s", ""},
	}, {
		name:   "match arm inside a bracket of the signature",
		source: "theorem m (h : (match n with\n  | 0 => True\n  | _ => False)) : True := trivial\n",
		text:   map[string]string{"m": "theorem m (h : (match n with | 0 => True | _ => False)) : True := trivial"},
		owners: []string{"sig m", "sig m", "sig m", ""},
	}, {
		name: "a macro's expansion after => is its body",
		source: "local macro \"my_decide\" l:ident : tactic =>\n" +
			"  `(tactic| (rw [$l:ident, data_eq]; decide))\n" +
			"\n" +
			"mirror_cmd Data.check 1 check_m1\n" +
			"\n" +
			"elab \"my_tac\" : tactic => do\n" +
			"  pure ()\n" +
			"theorem after : True := trivial\n",
		text: map[string]string{
			`"my_decide"`: `local macro "my_decide" l:ident : tactic =>`,
			`"my_tac"`:    `elab "my_tac" : tactic => do`,
			"after":       "theorem after : True := trivial",
		},
		owners: []string{`sig "my_decide"`, `body "my_decide"`, "", "", "", `sig "my_tac"`, `body "my_tac"`, "sig after", ""},
	}, {
		name: "an unindented word after a complete term starts a command",
		source: "axiom ax : Nat\n" +
			"register_thing ax\n" +
			"def d : Nat :=\n" +
			"  f 1\n" +
			"mirror_cmd d 1 d_m1\n" +
			"theorem after : True := trivial\n",
		text:   map[string]string{"ax": "axiom ax : Nat", "d": "def d : Nat :=", "after": "theorem after : True := trivial"},
		owners: []string{"sig ax", "", "sig d", "body d", "", "sig after", ""},
	}, {
		name: "unindented lines that continue a body",
		source: "def d : Nat :=\n" +
			"  f 1 +\n" +
			"g 2\n" +
			"def l : List Nat := [1,\n" +
			"two, 3]\n" +
			"def r : Nat := go 3\n" +
			"where\n" +
			"  go n := n\n" +
			"termination_by 0\n" +
			"theorem after : True := trivial\n",
		text:   map[string]string{"d": "def d : Nat :=", "l": "def l : List Nat := [1,", "r": "def r : Nat := go 3", "after": "theorem after : True := trivial"},
		owners: []string{"sig d", "body d", "body d", "sig l", "body l", "sig r", "body r", "body r", "body r", "sig after", ""},
	}, {
		name: "syntax commands end an open signature",
		source: "syntax \"one\" : tactic\n" +
			"declare_syntax_cat pat\n" +
			"syntax \"two\" : pat\n" +
			"syntax (ident <|> \"_\") : pat\n" +
			"syntax \"three\" : tactic\n" +
			"macro_rules\n" +
			"  | `(tactic| three) => `(tactic| trivial)\n" +
			"syntax \"four\" : tactic\n" +
			"elab_rules : tactic\n" +
			"  | `(tactic| four) => pure ()\n" +
			"theorem after : True := trivial\n",
		text: map[string]string{
			`"one"`:   `syntax "one" : tactic`,
			`"two"`:   `syntax "two" : pat`,
			`"three"`: `syntax "three" : tactic`,
			`"four"`:  `syntax "four" : tactic`,
			"after":   "theorem after : True := trivial",
		},
		owners: []string{`sig "one"`, "", `sig "two"`, "", `sig "three"`, "", "", `sig "four"`, "", "", "sig after", ""},
	}} {
		t.Run(test.name, func(t *testing.T) {
			got := ScanDeclarations("Signatures.lean", test.source)
			if len(got) != len(test.text) {
				t.Fatalf("ScanDeclarations() = %+v, want %d declarations", got, len(test.text))
			}
			for _, declaration := range got {
				if want, found := test.text[declaration.Name]; !found || declaration.Text != want {
					t.Errorf("%s Text = %q, want %q", declaration.Name, declaration.Text, want)
				}
			}
			owners := ScanLineOwners("Signatures.lean", test.source)
			shape := make([]string, len(owners))
			for index, owner := range owners {
				switch {
				case owner.Owner.Name == "":
				case owner.IsSignature:
					shape[index] = "sig " + owner.Owner.Name
				default:
					shape[index] = "body " + owner.Owner.Name
				}
			}
			if !sameStrings(shape, test.owners) {
				t.Errorf("line owners = %q, want %q", shape, test.owners)
			}
		})
	}
}

// An anonymous instance whose generated name the scanner can compute exactly
// is recorded under it: inst, the constants of its type, and Of with the
// constants of each binder its type does not depend on. A section variable
// contributes nothing, as in Lean. The expected names are what Lean 4.35
// generated for this source. The type may be on a continuation line, after
// modifiers and a priority.
func TestScanDeclarationsNamesAnonymousInstances(t *testing.T) {
	source := "variable {GF : Type} [Group GF]\n" +
		"namespace Alpha\n" +
		"instance : Inhabited Point := ⟨⟨0⟩⟩\n" +
		"noncomputable instance (priority := low) (G : Type) [Group G] :\n" +
		"    Inhabited (Box G) where\n" +
		"  default := ⟨0⟩\n" +
		"@[simp] instance {n : Nat} : Data.Shape.Decidable (Fits n) := ⟨true⟩\n" +
		"instance named : Inhabited Unit := ⟨()⟩\n" +
		"instance : Shape (Box GF) := ⟨fun _ => 0⟩\n" +
		"end Alpha\n" +
		"namespace Gamma\n" +
		"instance : Shape Point := ⟨fun p => p.x⟩\n" +
		"end Gamma\n"
	want := []Declaration{
		{File: "Instances.lean", Name: "Alpha.instInhabitedPoint", Text: "instance : Inhabited Point := ⟨⟨0⟩⟩"},
		{File: "Instances.lean", Name: "Alpha.instInhabitedBoxOfGroup", Text: "noncomputable instance (priority := low) (G : Type) [Group G] : Inhabited (Box G) where"},
		{File: "Instances.lean", Name: "Alpha.instDecidableFits", Text: "@[simp] instance {n : Nat} : Data.Shape.Decidable (Fits n) := ⟨true⟩"},
		{File: "Instances.lean", Name: "Alpha.named", Text: "instance named : Inhabited Unit := ⟨()⟩"},
		// GF is a section variable, so it adds nothing to the name.
		{File: "Instances.lean", Name: "Alpha.instShapeBox", Text: "instance : Shape (Box GF) := ⟨fun _ => 0⟩"},
		{File: "Instances.lean", Name: "Gamma.instShapePoint", Text: "instance : Shape Point := ⟨fun p => p.x⟩"},
	}
	got := ScanDeclarations("Instances.lean", source)
	if len(got) != len(want) {
		t.Fatalf("ScanDeclarations() = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("declaration %d = %+v, want %+v", index, got[index], want[index])
		}
	}
	owners := ScanLineOwners("Instances.lean", source)
	if owner := owners[5].Owner.Name; owner != "Alpha.instInhabitedBoxOfGroup" || owners[5].IsSignature {
		t.Errorf("line 6 owner = %q (signature %v), want the body of Alpha.instInhabitedBoxOfGroup", owner, owners[5].IsSignature)
	}
}

// The scanner does not elaborate, so for a shape whose generated name it
// cannot compute exactly it records a positional name that no Lean name can
// equal, rather than a guess that may be the name Lean gives another
// instance. Two anonymous instances whose computed name is the same are both
// positional: Lean numbers the second one by what the whole namespace already
// holds, which one file does not say.
func TestScanDeclarationsNamesUncomputableInstancesByPosition(t *testing.T) {
	source := "namespace Beta\n" +
		// A repeated base name: Lean gives instInhabitedPoint and
		// instInhabitedPoint_1, numbered by what the namespace already holds.
		"instance : Inhabited Point := ⟨⟨0⟩⟩\n" +
		"instance : Inhabited Point := ⟨⟨2⟩⟩\n" +
		// Notation: Lean names its constants (instInhabitedForallPoint,
		// instInhabitedProdPointNat).
		"instance : Inhabited (Point → Point) := ⟨id⟩\n" +
		"instance : Inhabited (Point × Nat) := ⟨(default, 0)⟩\n" +
		// A numeral: Lean names its OfNat (instInhabitedVecOfNatNat).
		"instance : Inhabited (Vec 1) := ⟨⟨0⟩⟩\n" +
		// A projection of a binder: Lean drops the binder (instTimelessRoom).
		"instance (st : St) (k : Nat) : Timeless (st.room k) := ⟨trivial⟩\n" +
		// An ascription: Lean ignores it (instTimelessWrapTrue).
		"instance : Timeless (wrap True : IProp) := ⟨trivial⟩\n" +
		// A computed name that a declaration of the same file already has:
		// Lean numbers the instance instShapeBox_1.
		"instance : Shape Box := ⟨fun _ => 0⟩\n" +
		"def instShapeBox : Nat := 0\n" +
		"end Beta\n"
	want := []string{
		"Beta.<anon-instance>#1",
		"Beta.<anon-instance>#2",
		"Beta.<anon-instance>#3",
		"Beta.<anon-instance>#4",
		"Beta.<anon-instance>#5",
		"Beta.<anon-instance>#6",
		"Beta.<anon-instance>#7",
		"Beta.<anon-instance>#8",
		"Beta.instShapeBox",
	}
	var got []string
	for _, declaration := range ScanDeclarations("Instances.lean", source) {
		got = append(got, declaration.Name)
	}
	if !sameStrings(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}
