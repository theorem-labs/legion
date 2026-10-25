# graphcheck

`graphcheck` reads a landing candidate before its build and prints the name and statement errors the build would
fail on, so that the landing program names a culprit in seconds instead of waiting for the build to go red.

```
graphcheck <names|statements|links|producers> --config FILE
```

It reads `LAND_TREE` (the candidate's git tree) and `LAND_BASE` (the batch's base commit) from the environment,
and prints one tab-separated finding line per hit, written by `landing.FormatGraphFinding` and read by
`landing.ParseGraphFinding`:

```
<verdict>	<kind>	<declaration>	<path>:<line>:<col>	<rule>: <message>
```

`verdict` is `refuse` (the build fails on it; the landing program returns the culprit before the build) or `flag`
(recorded, no member returned). `kind` is a `blame.Kind`: `unknown`, `added`, `changed` or `field`. stdout carries
finding lines and nothing else; stderr stays empty unless the command fails.

Exit codes: **0** decided, with or without findings; **2** a usage or configuration error; **3** unable to decide,
with one line on stderr saying why. The landing program records exit 3 as the verdict `unable` and goes on to the
build; any other nonzero exit fails the batch.

## How it reads Lean

A check that judges Lean code reads Lean's compiled output, never Lean source text with regular expressions
(stream lead, 2026-10-25). So:

- The **base** side comes from the declaration index (`declindex`): declarations, uses, imports, compiled
  statement hashes and compiled types of the base build.
- The **candidate** side is compiled. Each added or modified module under `roots` **that the build builds** is
  compiled with Lean (`declindex.CompileModule`), in import order, against the base build's outputs and the
  candidate modules already compiled, into a scratch directory outside every tree, at most 48 at a time. Each
  result is read with the index's own reader (`declindex.ParseModule`) and, for statements and types, with the
  index's exporter.
- Text is used for two things only, and judges nothing: `git diff` says **which** files changed, and a candidate
  file's line gives the **column** of a declaration whose line the compiled output records.

The check compiles the changed modules and nothing downstream of them, so it ends well before the build it runs
in front of. It prints no timing: the landing program records each check's wall time in `CheckVerdict.Seconds`.

## The rules

| Rule | Verdict | What it says |
| --- | --- | --- |
| C1 | refuse | A changed module the build builds does not compile. |
| N1 | refuse | A built module that the batch does not change uses a name the candidate no longer declares. |
| N2 | refuse | A name the candidate adds is declared by another built module too. |
| S2 | flag | A new declaration restates one the candidate already has. |
| L1 | flag | A link's theorem keeps a contract hypothesis that another directory's link concludes. |
| P1 | flag | A binder names an interface that no producer has. |

**C1** (`compile.go`). A changed module that Lean refuses is a certain build failure. `blame.Parse` reads the
compile output with the configured roots, and every declaration it names becomes one refusal, with blame's kind,
at the error's file, line and column, and the rule text `C1:` followed by Lean's first error line. The landing
program then attributes it with `SelectFor` exactly as it attributes its build blame, because the name and the
kind are the same ones its build blame would carry — which is why C1 reports the identifier as Lean wrote it, not
a name of ours. A failure that names no declaration makes the check unable (exit 3) for that module: never a
silent pass. C1 covers what a changed module does with a removed name, so there is no separate rule for it.

**N1** (`names.go`). A declaration the base had in a changed file, and the compiled candidate no longer has, is
removed. Each built module whose file the batch does not change and whose recorded uses name it gives one finding,
at that module's first use, counting the uses and the modules. A removed field is a removed declaration `S.f` of
a base structure or class `S` that the candidate still declares: compiled output has no notion of a field list, so
there is no text field list and no table of exceptions. A removal is a **flag** instead of a refusal when a use
may still resolve: another declaration of the candidate has the removed name's last component, or the structure of
a removed field now extends another structure, which may give the field back.

**N2** (`names.go`). A name a compiled candidate module declares that the base did not declare in that module, and
that another built module of the candidate declares too, is a refusal at the new declaration. Two declarations of
one name in one module are Lean's own error, so C1 reports that case.

**S2** (`statements.go`), **L1** (`links.go`) and **P1** (`producers.go`) are flags over compiled statements and
types: nothing is refused on them.

P1 is a name-level signal. It accepts an instance, a declaration whose conclusion is the interface, or a use of
the interface's constructor, so a producer written in a way it does not recognise is a **missed flag**, never a
false refusal.

## Configuration

`graphcheck.yaml` is read strictly: an unknown key is an error naming it. Relative paths are relative to the file.

```yaml
index: declindex.yaml       # the declindex configuration; graphcheck reads its db, tree, lib, roots and toolchain
repository: ..              # the git repository holding LAND_TREE and LAND_BASE
roots: [Lib]                # first components of the module names checked
build_roots: [Lib]          # the root modules the build builds; their import closure is "built"
root_new_modules: true      # the build also builds every module new in the candidate
statements: {}
links: {link_files: [], contract_name: "", exclude_files: [], open_marker: "", open_marker_min_reason: 0}
producers: {interface_names: [], binder_files: []}
```

The index's configuration carries everything the compile needs: `tree` (a checkout root), `lib` (the base build's
library directory), `jobs`, `lean.toolchain` and `lean.search_path`. `graphcheck` opens the index only through
`declindex.LoadConfig`, so moving the index to another store changes no code here.

## When the check cannot decide (exit 3)

- The index is incomplete, records no commit, or is of another tree than `LAND_BASE`'s.
- The index's tree is not a checkout root, so its module paths are not repository paths.
- `LAND_BASE` or `LAND_TREE` is unset or unknown to the repository.
- A changed module does not compile and Lean's output names no declaration.
- A configured input is missing or of the wrong format (exit 2 when the configuration file itself is wrong).

## Limits

- A changed module that no build target imports is not compiled and gives no finding: the build does not read it
  either, and refusing it would refuse a change the build accepts.
- N1 sees a use only where the base build recorded one. A use that the base's reference files do not record (an
  elaborated-away position) is not counted.
- A removed field is seen only where the index read the module with its compiled part (the exporter's constants):
  a field is the compiled constant `S.f`, and no module's declarations hold it. An index without that part gives
  no field finding — a miss, never a false refusal.
- The candidate side needs a Lean toolchain and the base build's `.olean` files. The package's rule tests build
  the fixture for real; without a toolchain they skip, and `GRAPHCHECK_REQUIRE_LEAN` turns that skip into a
  failure, as the `orchestration-lean` workflow does.
