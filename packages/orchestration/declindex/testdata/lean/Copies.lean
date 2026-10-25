/-! Copies of Fixture's `pickA` and `posA` in a module that does not import it, so Lean makes their auxiliary
definitions (`pickB.match_1`, `posB.proof_1`) anew. -/
namespace Fx

def pickB : Nat → Nat
  | 0 => 1
  | n + 1 => n
def posB : {n : Nat // n > 0} := ⟨1, by decide⟩

/-- The same shape about another user declaration: not a copy of Fx.Q. -/
def MyQ.base : Nat := 2
def MyQ : Prop := MyQ.base = 1

end Fx
