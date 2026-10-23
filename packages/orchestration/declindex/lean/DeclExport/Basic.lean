import Lean
open Lean

/-! Shared pieces of the exporter: numeric literals as values, and structural hashes of expressions that do not
depend on binder names, binder kinds, metadata or the names of universe parameters. -/

namespace DeclExport

/-- The value of a numeric literal however it is elaborated: a raw literal, `OfNat.ofNat _ n _`, `instOfNatNat n`
or `BitVec.ofNat w n` (the `n#w` notation). -/
partial def lit? (e : Expr) : Option Nat :=
  match e.consumeMData with
  | .lit (.natVal n) => some n
  | e =>
    if e.isAppOfArity ``OfNat.ofNat 3 then lit? (e.getArg! 1)
    else if e.isAppOfArity ``instOfNatNat 1 then lit? (e.getArg! 0)
    else if e.isAppOfArity ``BitVec.ofNat 2 then lit? (e.getArg! 1)
    else none

/-- Two independent 64-bit lanes, so that a hash collision among a million constants is not a concern. -/
structure H where
  a : UInt64
  b : UInt64
  deriving BEq, Hashable, Inhabited

def H.mix (x y : H) : H :=
  ⟨mixHash x.a y.a, (x.b * (0x100000001b3 : UInt64)) ^^^ (y.b + (0x9e3779b97f4a7c15 : UInt64) + (x.b <<< 6))⟩

def H.ofU (n : UInt64) : H := ⟨mixHash n 0x51ed27, n * (0xff51afd7ed558ccd : UInt64) + (0xc4ceb9fe1a85ec53 : UInt64)⟩

def H.ofString (s : String) : H := H.mix (H.ofU s.hash) (H.ofU s.length.toUInt64)

def H.tag (t : String) (xs : List H) : H := xs.foldl H.mix (H.ofString t)

def hex64 (n : UInt64) : String :=
  let s := String.ofList (Nat.toDigits 16 n.toNat)
  String.ofList (List.replicate (16 - s.length) '0') ++ s

def H.hex (h : H) : String := hex64 h.a ++ hex64 h.b

/-- How the hash treats names: a universe parameter by its position; the declaration itself, and the names Lean
generates from it (`X.eq_1`, `X._unfold`, `X.rec`, `X.match_1`), by a placeholder and the generated suffix, so
that a copy under a new name hashes equal; an auxiliary definition Lean makes while elaborating (a matcher, an
abstracted proof) by its content, since a copy elsewhere gets its own or reuses an existing one; any other
constant, a sub-declaration the user wrote (`X.base`) included, by its name. -/
structure HashCtx where
  levels : List Name := []
  self : List Name := []
  env? : Option Environment := none

/-- An auxiliary definition that elaboration made: a matcher, or a proof abstracted out of a body. -/
def isElabAux (env : Environment) (n : Name) : Bool :=
  Meta.isMatcherCore env n ||
    match n with
    | .str _ s => s.startsWith "proof_" || s.startsWith "match_" || s.startsWith "_auxLemma"
    | _ => false

/-- A name component Lean generates for a declaration's own parts: equation and unfolding lemmas, recursors and
the other parts of an inductive, the parts of structural and well-founded recursion, and compiler auxiliaries. -/
def generatedComponent (s : String) : Bool :=
  s.startsWith "_" ||
  ["rec", "recOn", "casesOn", "noConfusion", "noConfusionType", "below", "brecOn", "binductionOn", "ibelow",
   "sizeOf_spec", "eq_def", "eq_unfold", "injEq", "inj", "ctorIdx", "toCtorIdx", "ctorElim", "ctorElimType"].contains s ||
  ["match_", "proof_", "eq_", "spec_", "lambda_", "closed_", "elambda_"].any fun p =>
    s.startsWith p && s.length > p.length && (s.drop p.length).all Char.isDigit

/-- A suffix (the name after a declaration's) every component of which Lean generates. -/
def generatedSuffix : Name → Bool
  | .anonymous => true
  | .num p _ => generatedSuffix p
  | .str p s => generatedComponent s && generatedSuffix p

partial def hashLevel (ctx : HashCtx) : Level → H
  | .zero => H.tag "z" []
  | .succ l => H.tag "s" [hashLevel ctx l]
  | .max a b => H.tag "max" [hashLevel ctx a, hashLevel ctx b]
  | .imax a b => H.tag "imax" [hashLevel ctx a, hashLevel ctx b]
  | .param n => match ctx.levels.idxOf? n with
    | some i => H.tag "p" [H.ofU i.toUInt64]
    | none => H.tag "pn" [H.ofString n.toString]
  | .mvar _ => H.tag "m" []

structure HashState where
  memo : Std.HashMap Expr H := {}
  fvars : Std.HashMap FVarId Nat := {}

abbrev HashM := StateM HashState

/-- The structural hash of `e`. Numeric literals hash by value, so `7#64` and `OfNat.ofNat _ 7 _` agree. -/
partial def hashExpr (ctx : HashCtx) (e : Expr) : HashM H := do
  if let some h := (← get).memo[e]? then return h
  let h ← match e with
    | .bvar i => pure (H.tag "b" [H.ofU i.toUInt64])
    | .fvar id => do
      let s ← get
      let i ← match s.fvars[id]? with
        | some i => pure i
        | none => do
          let i := s.fvars.size
          set { s with fvars := s.fvars.insert id i }
          pure i
      pure (H.tag "f" [H.ofU i.toUInt64])
    | .mvar _ => pure (H.tag "?" [])
    | .sort l => pure (H.tag "S" [hashLevel ctx l])
    | .const n ls =>
      match ctx.env?.bind fun env => if isElabAux env n then env.find? n else none with
      | some ci =>
        let sub : HashCtx := { ctx with levels := ci.levelParams, self := n :: ctx.self }
        let parts : List H := Id.run (((ci.type :: ci.value?.toList).mapM (hashExpr sub)).run' {})
        pure (H.tag "aux" (parts ++ ls.map (hashLevel ctx)))
      | none =>
        let own := ctx.self.findIdx? fun p => p == n || (p.isPrefixOf n && generatedSuffix (n.replacePrefix p .anonymous))
        match own with
        | some i =>
          let suffix := (n.replacePrefix ctx.self[i]! .anonymous).toString
          pure (H.tag "self" (H.ofU i.toUInt64 :: H.ofString suffix :: ls.map (hashLevel ctx)))
        | none => pure (H.tag "c" (H.ofString n.toString :: ls.map (hashLevel ctx)))
    | .app .. =>
      match lit? e with
      | some n => pure (H.tag "n" [H.ofString (toString n)])
      | none => do pure (H.tag "a" [← hashExpr ctx e.appFn!, ← hashExpr ctx e.appArg!])
    | .lam _ d b _ => do pure (H.tag "L" [← hashExpr ctx d, ← hashExpr ctx b])
    | .forallE _ d b _ => do pure (H.tag "P" [← hashExpr ctx d, ← hashExpr ctx b])
    | .letE _ t v b _ => do pure (H.tag "let" [← hashExpr ctx t, ← hashExpr ctx v, ← hashExpr ctx b])
    | .lit (.natVal n) => pure (H.tag "n" [H.ofString (toString n)])
    | .lit (.strVal s) => pure (H.tag "str" [H.ofString s])
    | .mdata _ b => hashExpr ctx b
    | .proj s i b => do pure (H.tag "pj" [H.ofString s.toString, H.ofU i.toUInt64, ← hashExpr ctx b])
  -- A free variable's hash depends on the order of first occurrence, so terms with free variables are not memoised.
  if !e.hasFVar then modify fun s => { s with memo := s.memo.insert e h }
  return h

def hashOf (ctx : HashCtx) (es : List Expr) : H :=
  (es.mapM (hashExpr ctx) |>.run' {}).foldl H.mix (H.tag "stmt" [])

/-- The relation heads whose applications are atoms of a statement. -/
def relationHeads : List Name :=
  [``Eq, ``Ne, ``HEq, ``Iff, ``LT.lt, ``LE.le, ``GT.gt, ``GE.ge, ``Dvd.dvd, ``Membership.mem]

def symmetricHeads : List Name := [``Eq, ``Ne, ``Iff, ``HEq]

/-- The canonical hash of one atom: free variables by first occurrence, and a symmetric relation hashed in the
order of its two hashes, so that `a = b` and `b = a` agree. -/
def atomHash (e : Expr) : H :=
  let e := e.consumeMData
  let one (x : Expr) : H := hashOf {} [x]
  match e.getAppFn.consumeMData with
  | .const n _ =>
    if symmetricHeads.contains n && e.getAppNumArgs ≥ 2 then
      let args := e.getAppArgs
      let l := args[args.size - 2]!
      let r := args[args.size - 1]!
      let swapped := mkAppN e.getAppFn (args.set! (args.size - 2) r |>.set! (args.size - 1) l)
      let h1 := one e
      let h2 := one swapped
      if h1.a ≤ h2.a then h1 else h2
    else one e
  | _ => one e

end DeclExport
