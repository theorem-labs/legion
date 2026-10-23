# The declaration index and the similarity gate

`cmd/declindex` answers, for a built Lean project:

- where a name is declared (`where`);
- who uses it, and how far a change to its statement reaches (`users`);
- its statement hash (`hash`), the hash a change lock records for its reads, and the same for the landing program (`land-hash`);
- who states a registered fact (`states`), and who shares a relation atom with a statement (`same`).

It also runs the similarity gate in front of the librarian (`gate`), and after a landing it checks the contract ledger (`ledger`).

## Parts

- **The `.ilean` part** (`ilean.go`, `refresh.go`, `query.go`, `graph.go`). It reads Lean's reference files: declarations, kinds, positions, uses marked `stmt` or `proof`, and edges.
  - A module is read again when its `.ilean` (Lake's content hash) or its source (size and mtime) changes.
  - A module whose source is newer than its build (a declaration's recorded position no longer holds its name) is unreadable: the refresh reports it, and queries and the ledger check are unable to check (exit 3) until a build brings it back. Names Lean makes for a command (an elaborator's auxiliary, an initializer) have no written name and are not compared.
  - Reach walks a compressed graph file written next to the SQLite file. An edge's target is counted as the longest declared name it extends (`X.wp` is the field of `X`), against every module's declarations when the graph is written, so an incremental refresh answers as a full one does.
  - The graph file carries the generation of the index it was built from. A query refuses a graph of another generation, and the next refresh writes it again, so a failed write is never read.
- **The compiled part** (`lean/`, `export.go`, `compiled.go`). The exporter is a Lean program, built with the tree's own toolchain. It reads the `.olean` files and records, for each constant:
  - its printed type;
  - a compiled statement hash. Binder names, universe names, the declaration's own name and the names Lean generates from it (`X.eq_1`, `X._unfold`, `X.rec`) are erased, a sub-declaration the user wrote (`X.base`) is kept by name, an auxiliary definition Lean makes while elaborating (a matcher, an abstracted proof) is hashed by its content, and literals are compared by value. A structure's field names are kept;
  - relation atoms;
  - the registry facts its statement states: `direct`, `via:<definition>`, or `uses`.
- **Matching facts.** A form matches as a first-order pattern, in either order for `=`, `≠`, `↔` and `HEq`, and through local aliases.
  - A definition's templates carry a match through applications of any depth.
  - After a landing, the modules whose `.olean` changed are read again, and so are the statements reached from a definition whose compiled hash changed. Every module is read again when the exporter, the forms, or the body of a concept with no form changes.
- **The registry** (`conceptgate/registry.go`) is a YAML file in the project's git. Each concept has:
  - a name;
  - a home file;
  - a one-sentence meaning;
  - `extends`, when it extends another concept;
  - forms: Lean lambda terms whose binders are pattern variables. A concept with no form is matched by its declaration's body;
  - `compiled_hash`, which the gate saves when it adds a concept.

## The gate's rules and why

The rules come from a measurement on a large project of one fact restated across it (a register being 8-aligned): how much of its restatements each method found, against a rule-based ground truth.

| Verdict (exit code) | Rule | Why |
| --- | --- | --- |
| name-taken (5) | The name is a registered concept's or an existing declaration's. | The statement may be new; the proposal needs another name. |
| refused (1) | The compiled statement hash equals a registered concept's (its compiled entry in the index, or the hash saved when the gate added it), then any declaration's. The verdict names it. | The exact methods were 100% precise, so an exact duplicate needs no judgment. |
| near (4) | The proposal's own text states a registered form, directly or through a definition the proposal declares. Or a registered concept cannot be compared: it has no compiled entry, no saved hash and no form. | Compiled statements followed through definitions found 98% of the restatements at 99% precision; text normalisation found at most 85%. A concept the gate cannot compare goes to the librarian, never past it. |
| added (0) | Anything else, including a proposal that only applies a registered concept or an existing definition. | Reuse is the wanted outcome. |

Two thresholds are deliberately absent:

- **No embedding threshold.** Embeddings reached ROC-AUC 0.80 on this fact, and no threshold reached 90% precision.
- **No partial-atom threshold.** A threshold of half the atoms would make every inline statement of a fact near the definition that bundles it with another.

A proposal's own definition that only wraps a registered concept goes to the librarian. The matcher cannot tell such a template from a restatement, and a false near match costs only a review.

The gate cannot decide (exit 3) when the index lacks its compiled part, when the proposal does not compile, or when the walk of a statement hits its node budget.

### Untrusted proposals

Compiling a proposal must run none of its code outside a sandbox, so the gate checks it three times:

1. **A text filter** refuses the obvious at once (`#eval`, `run_cmd`, `initialize`, macros, `unsafe`, …). It is fast and nothing relies on it: a character literal `'"'` or an unlisted form passes it.
2. **Lean's own parser** (`declexport --check`) reads the proposal with the notation of the modules it imports and allows only declarations, `variable`, `universe`, module documentation, `attribute` with listed attributes, and the scoping commands (`namespace`, `section`, `end`, `open`), which alone it elaborates. Inside them it allows only listed attributes (`simp`, `instance`, `reducible`, …), refuses `unsafe`, and refuses the forms that run code while elaborating: `by_elab`, `run_tac`, `native_decide`, `decide +native`. A proposal carries no forms: the librarian writes those.
3. **An operating-system sandbox** (package `confine`) runs the check, the compile and the export. It needs no privilege and no user namespace, and it needs Landlock ABI 3 (Linux 6.2), the first that governs truncation:
   - **Files.** Landlock lets the process read only the toolchain, the build outputs it imports and its scratch directory, and write only the scratch directory. It never grants REFER, so nothing is linked or renamed into or out of the scratch directory. The scratch directory is fresh for each step (this user's, mode 0700, and empty when the step starts, so no entry placed beforehand, such as a hard link to an outside file, can be in it), and the inputs are copied into it.
   - **Processes.** The program is its only process. The seccomp filter sends the launcher's one exec of the program to the parent, which lets it through and closes the channel; the kernel then fails every later `execve` and `execveat`, so neither another program, Lean itself, nor the dynamic loader running a copied binary can start. `clone` succeeds only for a thread, `clone3` reports ENOSYS (the C library falls back to `clone`), and `fork` and `vfork` fail.
   - **The rest.** Every `socket(2)`, io_uring, ptrace, `process_vm_*`, key, bpf, namespace and mount call fails, and the environment is `PATH`, `LEAN_SYSROOT`, `LEAN_PATH`, `HOME` and `TMPDIR` only.

   Where the sandbox cannot be built (no Landlock, or an ABI before 3), the gate is unable to decide (exit 3) and compiles nothing.

The initializers of the modules a proposal imports (the project's own, and its own elaborators and tactics) run inside the sandbox when the proposal is checked and compiled. That is acceptable because the sandbox confines them as it does the proposal: they can read only what the step reads, write only its fresh scratch directory, and start no process. A proposal cannot define an initializer of its own: the parser check refuses `initialize`, `builtin_initialize` and the `init` attributes.

## The fixture

`testdata/lean/Fixture.lean` states one fact, register x2 of a register file being 8-aligned (`Fx.SpAligned`), in each way the matcher must find: inline, through a field projection, with sides swapped, through a local alias, with an offset, as `% 8`, through wrappers (`Fx.Frame`, `Fx.Wrap`) of any depth; and statements that look alike and are not it.

## Measured on a large project

Measured on a large built project (52,529 modules, 545,934 declarations), 48 jobs:

- **Parity with the reference `.ilean` reader:** the same 545,934 declarations, 9,686,627 uses and 4,389,369 resolved edges, with 0 differences. For 55 declarations sampled across wide and narrow use, the same direct users, per-directory counts and use lines; reach differs only for the 4 whose users include `example`s, which this index counts once per module.
- **Refresh from nothing:** 15 min 43 s; 2 min 3 s for the `.ilean` part, 13 min 40 s for the compiled part (789,877 constants).
- **Update after a landing:** 2.2 s when nothing changed; 3.4 s when one compiled module changed; about 16 s when a reference file changed, 13.5 s of which rebuilds the graph file.
- **Query times, each process included:** `states` of the fact's concept 0.34 s (23,095 rows); `users` 0.2 s median and 0.67 s worst; `where` 0.014 s; `same` 0.11 s; `gate` about 5 s, which includes the parser check and compiling the proposal in the sandbox.
- **`states` against the rule's 1,824 files that restate the fact:** 98.1% (1,789) recall through definitions; 66.1% from direct statements only. It also finds 88 files that the rule lacks.
- **The gate:** a copy of a registered concept was refused, naming it; a restatement of the fact went to the librarian, the only message it received; a new concept was added with its compiled hash; proposals with an `#eval` that writes a file, the same hidden behind a character literal `'"'`, and a `by_elab` that writes a file were each refused (the first by the text filter, the other two by Lean's parser) and no file was written.

## The ledger

`ledger` runs after a landing on the index of the landed build.

- It reads the ledger and writes only to an existing one: a database without the ledger's schema or the repository is refused (exit 3).
- It registers each contract that the rule selects and that has an owner, at version 1 with the statement hash. Owners come from the owner map (`owners`): `{"format": F, "owners": [{"id": <owner key>, "contract": <directory> | null}, …]}`, where `owners_format` names the `F` the deployment expects.
- It reports each contract whose ledger hash differs from the index's, and each ledger contract the index no longer declares (exit 1). It never advances a contract: only `Land` does.
- The hash is the text statement hash, because a change lock's reads carry that hash and `Land` compares the two.
