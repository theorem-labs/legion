package conceptgate

import "testing"

func TestCheckProposalSource(t *testing.T) {
	for _, ok := range []string{
		"import Fixture\n\ndef A (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64\n",
		"namespace X\nopen Fx\nstructure B where\n  prefix : Nat\ndef c (b : B) : Nat := b.prefix + #[1, 2].size\nend X\n",
		"/-- `#eval` and `run_cmd` in a docstring -/\ntheorem t : True := trivial -- #eval in a comment\n",
		"def s : String := \"#eval run_cmd\"\n",
		"@[simp] theorem u : 1 + 1 = 2 := rfl\n",
		// Iris proof-mode patterns are tactic syntax, not commands: inline, and on a line of their own where a bracketed
		// pattern goes on past the end of a line.
		"theorem t (P : Prop) : True := by\n  iintro ⟨#Hinv, HR⟩\n  icases H with ⟨Hctx, #H0⟩\n  iintro #H\n  trivial\n",
		"theorem t : True := by\n  iintro ⟨%pa, %p, %⟨hw, -, hram⟩,\n    #Hinv, Hq⟩\n  trivial\n",
		"instance : Inhabited Nat where\n  default := by iintro #Hw; exact 0\n",
		"theorem t : True := by\n  iIntro \"#H\"\n  trivial\n",
		// A literal whose syntax starts with `#` (a vector's `#v[…]`), first on a line where its term goes on.
		"def v : Vector Nat 2 :=\n  #v[1, 2]\n",
	} {
		if err := checkProposalSource(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"#eval IO.println \"x\"\n",
		"def a := 1\n  #print a\n",
		// A command after a proof that uses Iris patterns, and one after a closed bracket.
		"theorem t : True := by\n  iintro #H\n  trivial\n#eval IO.println \"x\"\n",
		"def p := (1,\n  2)\n  #print p\n",
		"run_cmd pure ()\n",
		"run_meta pure ()\n",
		"run_elab pure ()\n",
		"initialize pure ()\n",
		"builtin_initialize pure ()\n",
		"macro \"x\" : term => `(1)\n",
		"elab \"x\" : term => pure default\n",
		"syntax \"x\" : term\n",
		"notation \"x\" => 1\n",
		"@[init foo] def bar : IO Unit := pure ()\n",
		"attribute [init foo] bar\n",
		"@[implemented_by foo] def bar : Nat := 1\n",
		"@[extern \"c\"] def bar : Nat := 1\n",
		"unsafe def bar : Nat := 1\n",
		"theorem t : 2 + 2 = 4 := by native_decide\n",
		"theorem t : True := by run_tac pure ()\n",
	} {
		if err := checkProposalSource(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
