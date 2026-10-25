import Golden

/-! A second module: uses across modules, a wrapper definition, and reach through it. -/
namespace Gold2
open Gold

/-- The frame at the stack pointer of a register file. -/
def FrameAt (R : BitVec 5 → BitVec 64) : Prop := Frame (R 2#5)

theorem frameAt_al (R : BitVec 5 → BitVec 64) (h : FrameAt R) : Aligned (R 2#5) := h.al

theorem frameAt_use (R : BitVec 5 → BitVec 64) (h : FrameAt R) : True := by
  have _ := h.lo
  trivial

theorem only_proof : (16 : Nat) ≤ 16 := by
  have _ := @Frame.lo
  exact Nat.le_refl _

theorem box_two : useBox ⟨2, by decide⟩ = 3 := rfl

end Gold2
