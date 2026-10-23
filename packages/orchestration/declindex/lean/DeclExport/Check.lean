import Lean
open Lean Elab

/-! Whether a proposal is declarations only, read with Lean's own parser and the notation of the modules it
imports, before anything elaborates it.

A proposal may hold declarations (`def`, `theorem`, `structure`, …), `variable` and `universe`, module
documentation, and the scoping commands `namespace`, `section`, `end` and `open`. Only the scoping commands are
elaborated here, so that the notation they bring into scope parses; they run no code of the proposal's.
Every other command is refused (`#eval`, `run_cmd`, `initialize`, `macro`, `syntax`, `notation`, `elab`,
`attribute`, `set_option`, `open … in`, …). Inside the allowed commands, the attributes must be on an allow list,
`unsafe` is refused, and so are the term and tactic forms that run code while elaborating: `by_elab`,
`run_tac`, `native_decide` and `decide +native`. -/

namespace DeclExport

def allowedCommands : List Name :=
  [``Parser.Command.declaration, ``Parser.Command.variable, ``Parser.Command.universe,
   ``Parser.Command.moduleDoc]

def scopingCommands : List Name :=
  [``Parser.Command.namespace, ``Parser.Command.section, ``Parser.Command.end, ``Parser.Command.open]

/-- Attributes that only tag a declaration for the elaborator: none registers code to run. -/
def allowedAttributes : List String :=
  ["simp", "instance", "reducible", "irreducible", "semireducible", "inline", "noinline", "specialize", "ext",
   "class", "default_instance", "match_pattern", "elab_as_elim", "induction_eliminator", "cases_eliminator",
   "refl", "symm", "trans", "congr", "deprecated", "grind", "local", "scoped"]

/-- Syntax kinds whose elaboration evaluates code of the proposal's. -/
def runningKinds : List String := ["byElab", "runTac", "runElab", "runMeta", "runCmd", "nativeDecide", "evalTac"]

partial def hasIdent (stx : Syntax) (n : String) : Bool :=
  match stx with
  | .ident _ _ v _ => v.toString == n
  | .node _ _ args => args.any (hasIdent · n)
  | _ => false

/-- The first reason the syntax tree runs code or carries a refused attribute or modifier. -/
partial def badSyntax (stx : Syntax) : Option String :=
  match stx with
  | .node _ kind args =>
    let last := match kind with | .str _ s => s | _ => ""
    if runningKinds.contains last then some s!"`{kind}` runs code while it elaborates"
    else if last == "decide" && hasIdent stx "native" then some "`decide +native` runs compiled code"
    else if kind == ``Parser.Term.attrInstance then
      let attr := args[1]!
      let name := match attr with
        | .node _ k as => if k == ``Parser.Attr.simple then as[0]!.getId.toString else
            match k with | .str _ s => s | _ => k.toString
        | _ => ""
      if allowedAttributes.contains name then args.findSome? badSyntax
      else some s!"the attribute `{name}` is not on the proposal allow list"
    else if kind == ``Parser.Command.unsafe then some "`unsafe` declarations are refused"
    else args.findSome? badSyntax
  | _ => none

/-- `none` when the proposal at path is declarations only; otherwise the reason it is refused. -/
unsafe def checkProposalUnsafe (path : String) : IO (Option String) := do
  let input ← IO.FS.readFile path
  let inputCtx := Parser.mkInputContext input path
  let (header, parserState, messages) ← Parser.parseHeader inputCtx
  if messages.hasErrors then return some "the header does not parse"
  -- The imported modules are the project's: their initializers run as they do when `lean` compiles the proposal.
  enableInitializersExecution
  let (env, messages) ← processHeader header {} messages inputCtx (trustLevel := 1024)
  if messages.hasErrors then
    return some s!"the imports do not load: {← messages.toList.mapM (·.toString)}"
  let mut state := Command.mkState env messages {}
  let mut ps := parserState
  repeat
    let scope := state.scopes.head!
    let pmctx := { env := state.env, options := scope.opts, currNamespace := scope.currNamespace,
                   openDecls := scope.openDecls }
    let (cmd, ps', msgs) := Parser.parseCommand inputCtx pmctx ps state.messages
    ps := ps'
    state := { state with messages := msgs }
    if msgs.hasErrors then
      return some s!"it does not parse: {← msgs.toList.mapM (·.toString)}"
    if Parser.isTerminalCommand cmd then
      if cmd.isOfKind ``Parser.Command.eoi then break
      return some "it holds a terminal command"
    let kind := cmd.getKind
    if scopingCommands.contains kind then
      let ctx : Command.Context := { fileName := path, fileMap := inputCtx.fileMap, snap? := none, cancelTk? := none }
      match ← ((Command.elabCommand cmd).run ctx |>.run state).toBaseIO with
      | .ok (_, s) => state := s
      | .error _ => return some s!"`{kind}` does not elaborate"
      if state.messages.hasErrors then return some s!"`{kind}` does not elaborate"
    else if allowedCommands.contains kind then
      if let some why := badSyntax cmd then return some why
    else
      return some s!"the command `{kind}` is not a declaration"
  return none

@[implemented_by checkProposalUnsafe] opaque checkProposal (path : String) : IO (Option String)

end DeclExport
