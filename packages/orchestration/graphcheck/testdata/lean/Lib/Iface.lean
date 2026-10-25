namespace Lib.Iface

class Sized (α : Type) where
  sz : α → Nat

structure Port where
  id : Nat

instance : Sized Nat := ⟨fun n => n⟩

end Lib.Iface
