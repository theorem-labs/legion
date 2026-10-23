/-! Statements of one fact, register x2 of a register file being 8-aligned, written in the ways a project writes
them, and statements that look alike but are not it. Core Lean only. -/
namespace Fx

/-- The registered concept. -/
def SpAligned (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 3#64 = 0#64

/-- A wrapper that asserts the alignment of its parameter. -/
def Frame (sp : BitVec 64) (n : Nat) : Prop := n ≤ 64 ∧ sp &&& 7#64 = 0#64

/-- Wrappers of the wrapper: one applies it to x2, one passes its own parameter on. -/
def Frame2 (R : BitVec 5 → BitVec 64) : Prop := Frame (R 2#5) 8
def Wrap (x : BitVec 64) : Prop := Frame x 16

/-- An internal variable equated with the parameter. -/
def Inner (x : BitVec 64) : Prop := ∀ y : BitVec 64, y = x → y &&& 7#64 = 0#64

structure Ctx where
  regs : BitVec 5 → BitVec 64

theorem inline (R : BitVec 5 → BitVec 64) (_h : R 2#5 &&& 7#64 = 0#64) : True := trivial
theorem inline_proj (k : Ctx) (_h : k.regs 2#5 &&& 7#64 = 0#64) : True := trivial
theorem swapped (R : BitVec 5 → BitVec 64) (_h : 0#64 = R 2#5 &&& 7#64) : True := trivial
theorem aliased (R : BitVec 5 → BitVec 64) (sp : BitVec 64) (_e : R 2#5 = sp) (_h : sp &&& 7#64 = 0#64) : True :=
  trivial
theorem offset (R : BitVec 5 → BitVec 64) (_h : (R 2#5 + 0xffffffffffffffc0#64) &&& 7#64 = 0#64) : True := trivial
theorem mod8 (R : BitVec 5 → BitVec 64) (_h : (R 2#5).toNat % 8 = 0) : True := trivial
theorem via_frame (R : BitVec 5 → BitVec 64) (_h : Frame (R 2#5) 32) : True := trivial
theorem via_frame2 (R : BitVec 5 → BitVec 64) (_h : Frame2 R) : True := trivial
theorem via_wrap (R : BitVec 5 → BitVec 64) (_h : Wrap (R 2#5)) : True := trivial
theorem via_inner (R : BitVec 5 → BitVec 64) (_h : Inner (R 2#5)) : True := trivial
theorem uses_concept (R : BitVec 5 → BitVec 64) (_h : SpAligned R) : True := trivial
theorem other_reg (R : BitVec 5 → BitVec 64) (_h : R 10#5 &&& 7#64 = 0#64) : True := trivial
theorem other_word (x : BitVec 64) (_h : x &&& 7#64 = 0#64) : True := trivial
theorem frame_other (x : BitVec 64) (_h : Frame x 8) : True := trivial

/-- Copies of the concept: the same text under a new name, and with its binder renamed. -/
def SpAlignedCopy (R : BitVec 5 → BitVec 64) : Prop := R 2#5 &&& 7#64 = 0#64
def SpAlignedRenamed (Q : BitVec 5 → BitVec 64) : Prop := Q 2#5 &&& 7#64 = 0#64

/-- A new definition that restates the concept inside a larger statement. -/
def NearSp (R : BitVec 5 → BitVec 64) (n : Nat) : Prop := n > 0 ∧ R 2#5 &&& 7#64 = 0#64

/-- A structure whose field states it. -/
structure SpBox (R : BitVec 5 → BitVec 64) : Prop where
  al : R 2#5 &&& 7#64 = 0#64

/-- Copies whose bodies Lean elaborates through auxiliary definitions (`pickA.match_1`, `posA.proof_1`). -/
def pickA : Nat → Nat
  | 0 => 1
  | n + 1 => n
def posA : {n : Nat // n > 0} := ⟨1, by decide⟩

/-- A statement about a user declaration under its own name. -/
def Q.base : Nat := 1
def Q : Prop := Q.base = 1

end Fx
