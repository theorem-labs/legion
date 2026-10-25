import Lib.Core

namespace Lib.Spec

structure SIZE_OK : Prop where
  holds : Lib.Core.size = 3

theorem size_ok : SIZE_OK := ⟨rfl⟩

end Lib.Spec
