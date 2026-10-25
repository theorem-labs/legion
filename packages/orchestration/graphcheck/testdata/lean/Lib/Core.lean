namespace Lib.Core

def helper (n : Nat) : Nat := n + 1

def size : Nat := 3

structure Box where
  val : Nat
  ok : val > 0 := by decide

end Lib.Core
