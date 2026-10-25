package conceptgate

import "testing"

func TestCheckProposalSource(t *testing.T) {
	for _, ok := range []string{
		"import Fixture\n\ndef A (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64\n",
		"namespace X\nopen Fx\nstructure B where\n  prefix : Nat\ndef c (b : B) : Nat := b.prefix + #[1, 2].size\nend X\n",
		"/-- `#eval` and `run_cmd` in a docstring -/\ntheorem t : True := trivial -- #eval in a comment\n",
		"def s : String := \"#eval run_cmd\"\n",
		"@[simp] theorem u : 1 + 1 = 2 := rfl\n",
	} {
		if err := checkProposalSource(ok); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"#eval IO.println \"x\"\n",
		"def a := 1\n  #print a\n",
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
