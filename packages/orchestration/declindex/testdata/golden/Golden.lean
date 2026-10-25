/-! Golden fixture: declarations whose statements and bodies exercise the reader. -/
namespace Gold

/-- A docstring with `:=` and a nested /- comment -/ inside. -/
def SpWord (R : BitVec 5 → BitVec 64) : BitVec 64 := R 2#5

/-- Aligned: the word is 8-aligned. -/
abbrev Aligned (x : BitVec 64) : Prop := x &&& 7#64 = 0#64

structure Frame (sp : BitVec 64) : Prop where
  al : Aligned sp
  lo : sp.toNat ≥ 16

theorem frame_al {sp : BitVec 64} (h : Frame sp) : Aligned sp := h.al

-- a line comment with := and where
theorem frame_lo (sp : BitVec 64) (h : Frame sp) :
    sp.toNat ≥ 16 := by
  exact h.lo

def pick : Nat → Nat
  | 0 => 1
  | n + 1 => n

def withStr (s : String := "a := b where") : String := s ++ "⟨\"x\"⟩"

instance instFrameDec (sp : BitVec 64) : Decidable (Aligned sp) := inferInstanceAs (Decidable (_ = _))

private def hidden (α₁ : Nat) : Nat := α₁ + 1

theorem uses_hidden : hidden 1 = 2 := rfl

/-- 𝔸 (astral plane) before a use: columns count UTF-16 units. -/ theorem astral (𝔸 : Nat) (h : 𝔸 = 1) : pick 𝔸 = 0 := by
  subst h; rfl

structure Box where
  val : Nat
  ok : val > 0 := by decide

def useBox (b : Box) : Nat := b.val + (Box.mk 1 (by decide)).val

theorem box_pos (b : Box) : useBox b > 0 := by
  unfold useBox; have := b.ok; omega

end Gold
