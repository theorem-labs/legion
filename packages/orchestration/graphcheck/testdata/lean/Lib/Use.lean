import Lib.Core

namespace Lib.Use

theorem helper_pos (n : Nat) : 0 < Lib.Core.helper n := by
  simp [Lib.Core.helper]

def boxVal (b : Lib.Core.Box) : Nat := b.val

def twice : Nat := Lib.Core.size * 2

end Lib.Use
