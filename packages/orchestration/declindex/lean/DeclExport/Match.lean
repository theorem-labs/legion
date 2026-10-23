import DeclExport.Basic
open Lean

/-! Which registered facts a statement states.

A *form* is a registered way to state a concept's fact: a lambda whose binders are pattern variables, for example
`fun (R : BitVec 5 → BitVec 64) => R 2#5 &&& 7#64 = 0#64`. A statement states the fact when one of its subterms is
an instance of a form:

* directly, in its own type (or a definition's value);
* through a definition it applies: each definition of the project gets, once, the *templates* of its body, the
  subterms that match a form once its parameters are given, and an application instantiates them with its
  arguments. Templates pass through definitions of any depth;
* through a local alias: a variable that the statement equates with a term stands for that term.

Matching is first-order and structural: a pattern variable matches any term, the same one each time; numeric
literals compare by value; `=`, `≠`, `↔` and `HEq` match in either order. Names, notation and namespaces play no
part. An application of a concept's own declaration is a *use* of the concept, recorded and not unfolded. -/

namespace DeclExport

structure Form where
  concept : String
  /-- Pattern variables are the free variables `_declexport.pv.i`. -/
  body : Expr
  arity : Nat
  head : Name

def pvPrefix : Name := `_declexport.pv

def pvar (i : Nat) : Expr := .fvar ⟨Name.mkNum pvPrefix i⟩

def pvIdx? (e : Expr) : Option Nat :=
  match e with
  | .fvar ⟨.num p i⟩ => if p == pvPrefix then some i else none
  | _ => none

def headName? (e : Expr) : Option Name :=
  match e.consumeMData.getAppFn.consumeMData with
  | .const n _ => some n
  | _ => none

/-- A form from a lambda term. -/
def Form.ofLambda (concept : String) (v : Expr) : Option Form := Id.run do
  let mut body := v.consumeMData
  let mut n := 0
  while body.isLambda do
    body := (body.bindingBody!.instantiate1 (pvar n)).consumeMData
    n := n + 1
  match headName? body with
  | some h => some { concept, body, arity := n, head := h }
  | none => none

/-- Match context: which free variables are wildcards (the parameters of the definition being summarised), and
the aliases of the statement's local variables. -/
structure MCtx where
  wild : Std.HashSet FVarId := {}
  aliases : Std.HashMap FVarId (Array Expr) := {}

abbrev Subst := Array (Option Expr)

/-- Match pattern `p` against term `t`. The Bool is true when a wildcard stood in for part of the pattern: the
match is complete only once the definition's arguments are known. -/
partial def pmatch (ctx : MCtx) (p t : Expr) (σ : Subst) (fuel : Nat := 3) : Option (Subst × Bool) :=
  let p := p.consumeMData
  let t := t.consumeMData
  match pvIdx? p with
  | some i =>
    match σ[i]? with
    | some none => some (σ.set! i (some t), false)
    | some (some u) => if u == t then some (σ, false) else none
    | none => none
  | none =>
    let viaAlias : Unit → Option (Subst × Bool) := fun _ =>
      match t with
      | .fvar id =>
        if fuel == 0 then none
        else (ctx.aliases[id]?.getD #[]).findSome? fun a => pmatch ctx p a σ (fuel - 1)
      | _ => none
    match t with
    | .fvar id =>
      if ctx.wild.contains id then some (σ, true)
      else match p with
        | .fvar id' => if id == id' then some (σ, false) else viaAlias ()
        | _ => viaAlias ()
    | _ =>
      match lit? p, lit? t with
      | some a, some b => if a == b then some (σ, false) else none
      | some _, none => none
      | none, some _ => none
      | none, none =>
        match p, t with
        | .app pf pa, .app tf ta => do
          let (σ, w1) ← pmatch ctx pf tf σ fuel
          let (σ, w2) ← pmatch ctx pa ta σ fuel
          some (σ, w1 || w2)
        | .const n _, .const m _ => if n == m then some (σ, false) else none
        | .sort _, .sort _ => some (σ, false)
        | .lit a, .lit b => if a == b then some (σ, false) else none
        | .bvar i, .bvar j => if i == j then some (σ, false) else none
        | .proj s i e, .proj s' i' e' => if s == s' && i == i' then pmatch ctx e e' σ fuel else none
        | .lam _ d b _, .lam _ d' b' _ | .forallE _ d b _, .forallE _ d' b' _ => do
          let (σ, w1) ← pmatch ctx d d' σ fuel
          let (σ, w2) ← pmatch ctx b b' σ fuel
          some (σ, w1 || w2)
        | _, _ => none

/-- `t` with the arguments of a symmetric relation swapped, if it is one. -/
def swapped? (t : Expr) : Option Expr :=
  let t := t.consumeMData
  match headName? t with
  | some n =>
    if symmetricHeads.contains n && t.getAppNumArgs ≥ 2 then
      let args := t.getAppArgs
      some (mkAppN t.getAppFn (args.set! (args.size - 2) args[args.size - 1]! |>.set! (args.size - 1) args[args.size - 2]!))
    else none
  | none => none

/-- Match a form against a term in either order of a symmetric relation. -/
def matchForm (ctx : MCtx) (f : Form) (t : Expr) : Option Bool :=
  let σ : Subst := Array.replicate f.arity none
  match pmatch ctx f.body t σ with
  | some (_, w) => some w
  | none => match swapped? t with
    | some t' => (pmatch ctx f.body t' σ).map (·.2)
    | none => none

/-- A template: a subterm of a definition's body that matches a form once the definition's parameters (loose
bound variables `0 … arity-1`, the last parameter innermost) are instantiated. -/
structure Template where
  atom : Expr
  arity : Nat
  deriving Inhabited

structure MatchConfig where
  forms : Array Form
  /-- The heads of all forms, for a quick test of a candidate. -/
  heads : Std.HashSet Name
  /-- Concept declarations: an application is a use of the concept. -/
  conceptDecls : Std.HashMap Name String
  roots : List Name
  budget : Nat := 3000000

structure St where
  next : Nat := 0
  summaries : Std.HashMap Name (Array Template) := {}
  inProgress : Std.HashSet Name := {}

abbrev M := StateRefT St MetaM

def inRoots (cfg : MatchConfig) (env : Environment) (c : Name) : Bool :=
  match env.getModuleIdxFor? c with
  | some idx => cfg.roots.contains (env.header.moduleNames[idx.toNat]!.getRoot)
  | none => false

def fresh : M Expr := do
  let s ← get
  set { s with next := s.next + 1 }
  return .fvar ⟨Name.mkNum `_declexport.x s.next⟩

/-- What one walk of a statement collects. -/
structure Walk where
  visited : Std.HashSet Expr := {}
  /-- Candidate subterms: an application whose head is a form head, and the definition it came through. -/
  cands : Array (Expr × Option Name) := #[]
  aliases : Std.HashMap FVarId (Array Expr) := {}
  uses : Array String := #[]
  atoms : Array Expr := #[]
  nodes : Nat := 0
  truncated : Bool := false

def Walk.alias (w : Walk) (v : FVarId) (t : Expr) : Walk :=
  let cur := w.aliases[v]?.getD #[]
  if cur.size ≥ 4 || cur.contains t then w else { w with aliases := w.aliases.insert v (cur.push t) }

mutual
/-- The templates of a definition of the project, computed once. -/
partial def summary (cfg : MatchConfig) (c : Name) : M (Array Template) := do
  if let some s := (← get).summaries[c]? then return s
  if (← get).inProgress.contains c then return #[]
  let env ← getEnv
  let some (.defnInfo d) := env.find? c | return #[]
  if !inRoots cfg env c then
    modify fun s => { s with summaries := s.summaries.insert c #[] }
    return #[]
  modify fun s => { s with inProgress := s.inProgress.insert c }
  let mut body := d.value
  let mut ps : Array Expr := #[]
  while body.isLambda do
    let fv ← fresh
    ps := ps.push fv
    body := body.bindingBody!.instantiate1 fv
  let w ← walk cfg body {}
  let wild : Std.HashSet FVarId := ps.foldl (fun s p => s.insert p.fvarId!) {}
  let ctx : MCtx := { wild, aliases := w.aliases }
  let mut out : Array Template := #[]
  for (e, _) in w.cands do
    -- An internal variable that the body equates with a term containing a parameter stands for that term.
    let e := e.replace fun x => match x with
      | .fvar id => if wild.contains id then none else
          (w.aliases[id]?.getD #[]).find? (fun a => a.hasAnyFVar (wild.contains ·))
      | _ => none
    if cfg.forms.any (fun f => f.head == (headName? e).getD .anonymous && (matchForm ctx f e).isSome) then
      let t : Template := { atom := e.abstract ps, arity := ps.size }
      if !out.any (·.atom == t.atom) then out := out.push t
  modify fun s => { s with summaries := s.summaries.insert c out, inProgress := s.inProgress.erase c }
  return out

/-- Walk `e`, opening binders with fresh variables, and collect candidates, aliases, uses and atoms. -/
partial def walk (cfg : MatchConfig) (e : Expr) (w : Walk) : M Walk := do
  if w.nodes > cfg.budget then return { w with truncated := true }
  if w.visited.contains e then return w
  let w := { w with nodes := w.nodes + 1, visited := w.visited.insert e }
  match e with
  | .mdata _ b => walk cfg b w
  | .proj _ _ b => walk cfg b w
  | .forallE _ d b _ | .lam _ d b _ =>
    let w ← walk cfg d w
    let fv ← fresh
    walk cfg (b.instantiate1 fv) w
  | .letE _ t v b _ =>
    let w ← walk cfg t w
    let w ← walk cfg v w
    walk cfg (b.instantiate1 v) w
  | .app .. =>
    let mut w := w
    let fn := e.getAppFn.consumeMData
    let args := e.getAppArgs
    if let .const c _ := fn then
      if cfg.heads.contains c then w := { w with cands := w.cands.push (e, none) }
      if relationHeads.contains c then w := { w with atoms := w.atoms.push e }
      if c == ``Eq && args.size == 3 then
        let l := args[1]!.consumeMData
        let r := args[2]!.consumeMData
        if l.isFVar && l != r then w := w.alias l.fvarId! r
        if r.isFVar && l != r then w := w.alias r.fvarId! l
      if let some concept := cfg.conceptDecls[c]? then
        if !w.uses.contains concept then w := { w with uses := w.uses.push concept }
      for t in ← summary cfg c do
        if t.arity ≤ args.size then
          w := { w with cands := w.cands.push (t.atom.instantiateRevRange 0 t.arity args, some c) }
    else
      w ← walk cfg fn w
    for a in args do
      w ← walk cfg a w
    return w
  | _ => return w
end

/-- A registered fact a statement states: the concept and how (`direct`, `via:<definition>` or `uses`). -/
structure Fact where
  concept : String
  how : String
  deriving BEq

def facts (cfg : MatchConfig) (w : Walk) : Array Fact := Id.run do
  let ctx : MCtx := { aliases := w.aliases }
  let mut out : Array Fact := #[]
  for (e, via) in w.cands do
    let some h := headName? e | continue
    for f in cfg.forms do
      if f.head == h && (matchForm ctx f e) == some false then
        let fact : Fact := { concept := f.concept, how := via.map (fun d => s!"via:{d}") |>.getD "direct" }
        if !out.contains fact then out := out.push fact
  for c in w.uses do
    let fact : Fact := { concept := c, how := "uses" }
    if !out.contains fact then out := out.push fact
  return out

end DeclExport
