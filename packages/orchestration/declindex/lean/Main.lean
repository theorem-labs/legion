import DeclExport.Match
import DeclExport.Check
open Lean Meta DeclExport

/-! `declexport MANIFEST.json`: load compiled modules (no elaboration, no build) and print one JSON line for each
constant of the target modules: its name `n`, module `m`, kind `k`, printed type `t`, compiled statement hash `h`,
value hash `vh` (definitions), relation atom hashes `a`, the registered facts it states `f`, and `tr` when the walk
hit its node budget. The manifest names the target modules, extra modules to import, the project's roots, the
forms (`{concept, const}`: a constant whose value is a form lambda), the concepts' own declarations, and the module
that the hashes count the target modules' constants as declared in (`home`). -/

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
  /-- When not empty, the module that the hashes count the constants of the target modules as declared in: a
  proposal, compiled as a module of its own, is hashed as of the module of its home file, where it lands. -/
  home : String := ""
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

/-- The names a copy of the declaration under another name would rename: itself (and so every name Lean makes
from it), and for a constructor its inductive. -/
def selfNames (ci : ConstantInfo) : List Name :=
  match ci with
  | .ctorInfo v => [ci.name, v.induct]
  | _ => [ci.name]

def truncate (s : String) (n : Nat := 4000) : String :=
  if s.length ≤ n then s else toString (s.take n) ++ " …"

/-- The binder names of a constructor's type: a structure's field names, which the hash otherwise erases. -/
partial def binderNames : Expr → List String
  | .forallE n _ b _ => n.toString :: binderNames b
  | _ => []

mutual
/-- Records in `ids` the identity that a reference to each constant of `e` counts in the hashes (`identify`), and,
for an auxiliary definition that the hash reads by content, of the constants of its type and value. -/
partial def identifyIn (ids : IO.Ref (Std.HashMap Name Name)) (base : Name → Name) (e : Expr) : MetaM Unit := do
  let env ← getEnv
  for c in e.getUsedConstants do
    if (← ids.get).contains c then continue
    match env.find? c with
    | some ci =>
      if isElabAux env c then
        ids.modify (·.insert c (base c))
        for x in ci.type :: ci.value?.toList do identifyIn ids base x
      else identify ids base c ci
    | none => ids.modify (·.insert c (base c))

/-- The identity of a reference to `n` in the hashes. Lean imports two public theorems of one name and type, or a
theorem and an axiom, or two axioms whose type is a proposition, from two modules as one constant, and keeps the
module that loaded first (Environment.lean, subsumesInfo): which one depends on which other modules the exporter
imports with it. So a reference to a public theorem, or to an axiom whose type is a proposition, counts by the hash
of its type, and any other constant, which Lean never merges, by the module that declares it (`base`). -/
partial def identify (ids : IO.Ref (Std.HashMap Name Name)) (base : Name → Name) (n : Name) (ci : ConstantInfo) :
    MetaM Unit := do
  ids.modify (·.insert n (base n))
  let merged ← match ci with
    | .thmInfo _ => pure !isPrivateName n
    | .axiomInfo _ => if isPrivateName n then pure false else (try Meta.isProp ci.type catch _ => pure false)
    | _ => pure false
  if !merged then return
  identifyIn ids base ci.type
  let tbl ← ids.get
  let h := hashOf { levels := ci.levelParams, moduleOf := fun c => tbl.getD c (base c) } [ci.type]
  ids.modify (·.insert n (Name.mkSimple s!"type:{h.hex}"))
end

/-- One JSON line for the constant `c` of the target module `m`. `ids` holds the identities that the hashes count
references by (`identify`), `base` the module of a constant that Lean does not merge. -/
def record (cfg : MatchConfig) (ids : IO.Ref (Std.HashMap Name Name)) (base : Name → Name) (types : Bool) (m : Name)
    (c : Name) (ci : ConstantInfo) : StateRefT St MetaM Json := do
  let env ← getEnv
  let exprs := statementExprs env ci
  let mut w : Walk := {}
  for e in exprs do
    w ← walk cfg e w
  for e in exprs ++ ci.value?.toList ++ w.atoms.toList do
    identifyIn ids base e
  let tbl ← ids.get
  let moduleOf : Name → Name := fun c => tbl.getD c (base c)
  let hctx : HashCtx := { levels := ci.levelParams, self := selfNames ci, env? := some env, moduleOf := moduleOf }
  let fields : List String := match ci with
    | .inductInfo v => v.ctors.flatMap fun k =>
        match env.find? k with
        | some kc => (binderNames kc.type).drop v.numParams
        | none => []
    | _ => []
  let h := fields.foldl (fun h n => H.mix h (H.ofString n)) (hashOf hctx exprs)
  let fs := facts cfg w
  let mut atoms : Array String := #[]
  for a in w.atoms do
    let s := (atomHash moduleOf a).hex
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
  if let ["--check", file] := args then
    initSearchPath (← findSysroot)
    match ← checkProposal file with
    | none => return 0
    | some why => IO.eprintln s!"declexport: the proposal is refused: {why}"; return 1
  let [path] := args | IO.eprintln "usage: declexport MANIFEST.json | declexport --check PROPOSAL.lean"; return 2
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
  -- The module of a constant that Lean does not merge, as the hashes count it: the one that declares it, or `home`
  -- for a target module. A merged one counts by its type (`identify`).
  let targets : NameSet := manifest.modules.foldl (fun s t => s.insert (toName t)) {}
  let base : Name → Name := fun n =>
    let m := match env.getModuleIdxFor? n with
      | some idx => env.header.moduleNames[idx.toNat]!
      | none => env.mainModule
    if !manifest.home.isEmpty && targets.contains m then toName manifest.home else m
  let ids ← IO.mkRef ({} : Std.HashMap Name Name)
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
        let j ← record cfg ids base manifest.types m c ci
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
