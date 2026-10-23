# The declaration index and the similarity gate

`cmd/declindex` answers, for a built Lean project:

- where a name is declared (`where`);
- who uses it, and how far a change to its statement reaches (`users`);
- its statement hash (`hash`), the hash a change lock records for its reads;
- who states a registered fact (`states`), and who shares a relation atom with a statement (`same`).

It also runs the similarity gate in front of the librarian (`gate`), and after a landing it checks the contract ledger (`ledger`).

## Parts

- **The `.ilean` part** (`ilean.go`, `refresh.go`, `query.go`, `graph.go`). It reads Lean's reference files: declarations, kinds, positions, uses marked `stmt` or `proof`, and edges. A module is read again when its `.ilean` (Lake's content hash) or its source (size and mtime) changes. Reach walks a compressed graph file written next to the SQLite file.
  - On the Linux proof's built tree this part gives the same rows as `uses`: 545,934 declarations, 9,686,627 uses and 4,389,369 edges.
  - The one difference in answers: reach counts each `example` once per module. Lean names every `example` `_example`.
- **The compiled part** (`lean/`, `export.go`, `compiled.go`). The exporter is a Lean program, built with the tree's own toolchain. It reads the `.olean` files and records, for each constant:
  - its printed type;
  - a compiled statement hash. Binder names, universe names and the declaration's own name are erased, and literals are compared by value. A structure's field names are kept;
  - relation atoms;
  - the registry facts its statement states: `direct`, `via:<definition>`, or `uses`.
- **Matching facts.** A form matches as a first-order pattern, in either order for `=`, `≠`, `↔` and `HEq`, and through local aliases.
  - A definition's templates carry a match through applications of any depth.
  - After a landing, the modules whose `.olean` changed are read again. So are the statements reached from a definition whose compiled hash changed.
- **The registry** (`conceptgate/registry.go`) is a YAML file in the project's git. Each concept has:
  - a name;
  - a home file;
  - a one-sentence meaning;
  - `extends`, when it extends another concept;
  - forms: Lean lambda terms whose binders are pattern variables. A concept with no form is matched by its declaration's body.

## The gate's rules and why

The data behind these rules is the close-match baseline of the stack-alignment fact: 1,824 Spec files defined by a text rule.

| Verdict | Rule | Why |
| --- | --- | --- |
| refused | The name is registered or declared already. Or the compiled statement hash equals that of a registered concept, then of any declaration. The verdict names it. | The exact methods were 100% precise, so an exact duplicate needs no judgment. The compiled hash is finer than the text hash: equal text that elaborates the same gives the same compiled statement. |
| near | The proposal's own text states a registered form, directly or through a definition the proposal declares. The definitions it reaches may be of any depth. | Compiled statements followed through definitions found 98.0% of the files at 99.2% precision. Text normalisation found at most 85%. |
| added | Anything else, including a proposal that only applies a registered concept or an existing definition. | Reuse is the wanted outcome. |

Two thresholds are deliberately absent:

- **No embedding threshold.** Embeddings reached ROC-AUC 0.797 on this fact, and no threshold reached 90% precision.
- **No partial-atom threshold.** A threshold of half the atoms would make every inline alignment near `StackFrame`.

A proposal's own definition that only wraps a registered concept goes to the librarian. The matcher cannot tell such a template from a restatement, and a false near match costs only a review.

## Measured on the Linux proof's tree

At commit `170372292`, on 48 jobs on the build machine:

- **Full build of the `.ilean` part:** 2 min 6 s.
- **Compiled part:** 15 min 3 s for 52,529 modules and 789,877 constants (the whole refresh from nothing, both parts: 18 min 26 s).
- **Update after a landing:**
  - 2.3 s when nothing changed;
  - 16.2 s when one module changed, 11 s of which rebuilds the graph file.
- **Query times, each process included:**
  - `users`: 0.009 s median and 0.59 s worst, for `kObj`;
  - `states` of the stack-alignment concept: 0.34 s for 23,095 rows;
  - `gate`: about 3.5 s, which includes compiling the proposal.
- **`states LinuxMmu.SK.SpAligned` against the rule's 1,824 Spec files:**
  - direct statements only: 66.1% recall;
  - through definitions as well: **98.1% (1,789/1,824)** recall.
  - It also finds 88 Spec files that the rule lacks. The baseline found that some of the rule's misses state the fact through `twStackIn` and the `*WarnRes` definitions.
  - Of the 35 files it misses, the baseline traced most to `k.sp` or to a parameter named `sp`, which no compiled form names, or to a missing or stale `.olean`.
- **The gate on the real tree:**
  - a copy of `StackFrame` was refused, naming `LinuxMmu.SK.StackFrame`;
  - a restatement of stack alignment was sent to the librarian, the only message it received;
  - a new concept was added.

## The ledger

`ledger` runs after a landing on the index of the landed build.

- It registers each contract that the rule selects and that has an owner. A contract's owner comes from an `owners/v1` file. Registration is at version 1, with the statement hash.
- It reports each contract whose ledger hash differs from the index's, and each ledger contract the index no longer declares. It never advances a contract: only `Land` does.
- On the Linux proof's slice it registered 420 owned contracts; the next run found all 435 owned contracts matching, 0 defects.
- The hash is the text statement hash, because a change lock's reads carry that hash and `Land` compares the two.
