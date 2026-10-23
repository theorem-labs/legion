import DeclExport.Match
open Lean Meta DeclExport

/-! `declexport MANIFEST.json`: load compiled modules (no elaboration, no build) and print one JSON line for each
constant of the target modules: its name `n`, module `m`, kind `k`, printed type `t`, compiled statement hash `h`,
value hash `vh` (definitions), relation atom hashes `a`, the registered facts it states `f`, and `tr` when the walk
hit its node budget. The manifest names the target modules, extra modules to import, the project's roots, the
forms (`{concept, const}`: a constant whose value is a form lambda) and the concepts' own declarations. -/

structure ConstRef where
  concept : String
  const : String
  deriving FromJson

structure Manifest where
  modules : Array String
  imports : Array String := #[]
  roots : Array String
  forms : Array ConstRef := #[]
  concepts : Array ConstRef := #[]
  types : Bool := true
  budget : Nat := 3000000
  deriving FromJson

/-- A dotted name as Lean prints it: a component in `«…»` may hold dots and other characters. -/
def toName (s : String) : Name := Id.run do
  let mut n : Name := .anonymous
  let mut cur := ""
  let mut quoted := false
  for c in s.toList do
    if c == '«' then quoted := true
    else if c == '»' then quoted := false
    else if c == '.' && !quoted then
      n := Name.mkStr n cur
      cur := ""
    else cur := cur.push c
  return Name.mkStr n cur

def kindOf : ConstantInfo → String
  | .defnInfo _ => "def" | .thmInfo _ => "theorem" | .inductInfo _ => "inductive" | .ctorInfo _ => "ctor"
  | .axiomInfo _ => "axiom" | .opaqueInfo _ => "opaque" | .recInfo _ => "rec" | .quotInfo _ => "quot"

/-- Constants the exporter skips: compiler and elaborator auxiliaries, not statements anyone wrote. -/
def auxiliary (env : Environment) (c : Name) (ci : ConstantInfo) : Bool :=
  ((privateToUserName? c).getD c).isInternal || isAuxRecursor env c || isNoConfusion env c ||
    ci matches .recInfo _ || Meta.isMatcherCore env c

def statementExprs (env : Environment) (ci : ConstantInfo) : List Expr :=
  match ci with
  | .defnInfo d => [d.type, d.value]
  | .opaqueInfo d => [d.type, d.value]
  | .inductInfo v => ci.type :: v.ctors.filterMap (fun c => (env.find? c).map (·.type))
  | _ => [ci.type]

/-- The names a copy of the declaration under another name would rename: itself, its constructors, its parent. -/
def selfNames (ci : ConstantInfo) : NameSet :=
  match ci with
  | .inductInfo v => v.ctors.foldl (·.insert ·) ({} : NameSet) |>.insert ci.name
  | .ctorInfo v => ({} : NameSet).insert ci.name |>.insert v.induct
  | _ => ({} : NameSet).insert ci.name

def truncate (s : String) (n : Nat := 4000) : String :=
  if s.length ≤ n then s else toString (s.take n) ++ " …"

/-- The binder names of a constructor's type: a structure's field names, which the hash otherwise erases. -/
partial def binderNames : Expr → List String
  | .forallE n _ b _ => n.toString :: binderNames b
  | _ => []

def record (cfg : MatchConfig) (types : Bool) (m : Name) (c : Name) (ci : ConstantInfo) : StateRefT St MetaM Json := do
  let env ← getEnv
  let exprs := statementExprs env ci
  let hctx : HashCtx := { levels := ci.levelParams, self := selfNames ci }
  let fields : List String := match ci with
    | .inductInfo v => v.ctors.flatMap fun k =>
        match env.find? k with
        | some kc => (binderNames kc.type).drop v.numParams
        | none => []
    | _ => []
  let h := fields.foldl (fun h n => H.mix h (H.ofString n)) (hashOf hctx exprs)
  let mut w : Walk := {}
  for e in exprs do
    w ← walk cfg e w
  let fs := facts cfg w
  let mut atoms : Array String := #[]
  for a in w.atoms do
    let s := (atomHash a).hex
    if !atoms.contains s then atoms := atoms.push s
  let ty ← if types then
      try pure (truncate (toString (← ppExpr ci.type)))
      catch _ => pure (truncate (toString ci.type))
    else pure ""
  let mut fields : List (String × Json) := [
    ("n", toJson c.toString), ("m", toJson m.toString), ("k", toJson (kindOf ci)), ("t", toJson ty),
    ("h", toJson h.hex), ("a", toJson atoms),
    ("f", Json.arr (fs.map fun f => Json.mkObj [("c", toJson f.concept), ("how", toJson f.how)]))]
  if let some v := ci.value? then
    fields := fields ++ [("vh", toJson (hashOf hctx [v]).hex)]
  if w.truncated then fields := fields ++ [("tr", toJson true)]
  return Json.mkObj fields

def main (args : List String) : IO UInt32 := do
  let [path] := args | IO.eprintln "usage: declexport MANIFEST.json"; return 2
  let manifest ← match Json.parse (← IO.FS.readFile path) >>= fromJson? (α := Manifest) with
    | .ok m => pure m
    | .error e => IO.eprintln s!"declexport: {path}: {e}"; return 2
  initSearchPath (← findSysroot)
  let t0 ← IO.monoMsNow
  let imports := (manifest.modules ++ manifest.imports).map fun s => ({ module := toName s } : Import)
  let env ← importModules imports {} (trustLevel := 1024)
  let t1 ← IO.monoMsNow
  IO.eprintln s!"declexport: imported {env.header.moduleNames.size} modules in {t1 - t0} ms"
  let mut forms : Array Form := #[]
  for r in manifest.forms do
    -- A concept with no written form is matched by its own declaration's body, when it has one.
    let own := r.const == r.concept
    match env.find? (toName r.const) with
    | some ci =>
      match ci.value?.bind (Form.ofLambda r.concept) with
      | some f => forms := forms.push f
      | none =>
        if !own then
          IO.eprintln s!"declexport: form {r.const} of {r.concept} is not a lambda with a constant head"; return 3
    | none =>
      if !own then IO.eprintln s!"declexport: form {r.const} of {r.concept} is not in the environment"; return 3
  let cfg : MatchConfig := {
    forms
    heads := forms.foldl (fun s f => s.insert f.head) {}
    conceptDecls := manifest.concepts.foldl (fun m r => m.insert (toName r.const) r.concept) {}
    roots := manifest.roots.toList.map toName
    budget := manifest.budget }
  let ctx : Core.Context := { fileName := "<declexport>", fileMap := default, maxHeartbeats := 0 }
  let out ← IO.getStdout
  let act : StateRefT St MetaM Nat := do
    let mut n := 0
    for ms in manifest.modules do
      let m := toName ms
      let some idx := env.getModuleIdx? m | throwError "module {m} is not loaded"
      for c in env.header.moduleData[idx.toNat]!.constNames do
        let some ci := env.find? c | continue
        if auxiliary env c ci then continue
        let j ← record cfg manifest.types m c ci
        out.putStrLn j.compress
        n := n + 1
    return n
  try
    let (n, _) ← ((act.run' {}).run' {} {}).toIO ctx { env }
    IO.eprintln s!"declexport: {n} constants in {(← IO.monoMsNow) - t1} ms"
    return 0
  catch e =>
    IO.eprintln s!"declexport: {e}"
    return 3
