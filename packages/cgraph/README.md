# cgraph

Builds one graph of a C program and its RISC-V image with three layers over the same nodes: calls,
accesses and structure links. It gives each function, data structure and subsystem one owner, and each
owner the message topics it subscribes to. The same command builds a few files or the whole program.

```bash
uv run cgraph build --config cgraph.toml --part slice --out out/slice --cache cache --jobs 16
```

Exit status: 0 built and every hard check passes; 1 a hard check failed (the outputs are still
written); 2 a usage or configuration error; 3 an input cannot be read.

## Sources

- **The C source**, parsed with libclang per translation unit, with the flags of a compilation
  database (`compile_commands.json`) or of `c.flags`. It gives accesses (read, write, lock, unlock,
  alloc, free), direct calls, calls through function-pointer fields, function addresses passed as
  arguments, callback tables, record layouts, and list or hash insertions. What the compiler does not
  evaluate is left out and kept with a reason: `sizeof` and `_Alignof` operands, the unselected
  associations of `_Generic`, `typeof` and cast types, branches whose condition folds to a constant,
  and const scalar globals.
- **The image** (ELF and `System.map`), decoded with capstone. Calls come from `jal`, `j`, and `jalr`
  through an `auipc`-set register. Data addresses come from `auipc`-based arithmetic, shared anchors
  and compressed loads included, and count only inside the declared size of an object symbol that
  `System.map` names. Thread-pointer accesses are per-CPU state.
- **The contract graph** (optional): the proof's own graph. It maps functions to contracts, confirms
  callback tables a C initializer also names, and gives the interfaces the measures map onto groups.

A helper defined in a header or a file is folded into a caller where the caller's machine code does
not call it, so the C layer sees what the compiler inlined. A helper the image calls stays a call and
becomes a function of the part.

## Nodes and ids

`function:<name>`, `struct:<name>`, `global:<name>` (a function-local static is `<function>.<name>`),
`percpu:<name>` and `subsystem:<name>`. A name with two definitions gets `@<file>` (and `:<line>` if
one file has both). Embedding and list or hash membership join data structures into groups. A value
type (no pointer at any depth, or listed in `c.value_types`) and a base that at least
`c.base_type_min_containers` types embed do not join. A group of two or more is a subsystem, named
from its largest struct by clang's size.

## Owners and topics

`owners.json` lists one owner per function of the part, per data structure that a part function
accesses or a structure link joins, per per-CPU state, and per subsystem. The owner's key is the node
id; `contract` is the contract graph's node, if any. `topics.json` gives each owner's subscriptions
under `topics.prefix`: `node.<id>.needs` and `all` for everyone; `node.<callee>.callers`,
`node.<caller>.callees` and `node.<data>.users` for a function; `node.<linked>.linked` for a data
structure or a subsystem; `node.<neighbour>.neighbors` for every neighbour in the three layers. In a
topic, the id has every character outside `[A-Za-z0-9_:-]` percent-encoded, so it stays one token.

## Checks

Each run writes `checks.json`: C calls against image calls, C accesses to globals against image
addresses, embeddings against clang's layout, and field offsets against the image's displacements
and constants. The hard checks fail the run: a parse error; a function of the part's files that the
image holds (by its symbols, or by the contract graph) but the graph lacks; a global the part's code
addresses but the graph lacks; an edge without provenance; an embedding without a layout; a
membership without a C site; no field offset that matches; an invalid or repeated id; a node without
exactly one owner; an invalid topic.

## Incremental builds

With `--cache`, each unit's facts are kept under a key of the tool's code, the vocabulary, the unit's
arguments and the contents of every file it included. A later build parses only the units whose key
changed, so after a landing it parses the changed files (and every unit including a changed header)
and gives the same bytes as a full build.

## Configuration

```toml
[inputs]
root = "/data"                       # optional; paths below are relative to it (or to this file)
c_tree = "src"
compile_commands = "src/compile_commands.json"   # or [c] flags
image = "build/program.elf"
system_map = "build/System.map"
contract_graph = "contracts.json"   # optional
contract_source = "proofs"           # optional: where the contract graph's file:line points

[parts.core]
files = ["lib/a.c", "lib/b.c"]       # or "all": every C file of compile_commands

[c]
flags = ["-I{c_tree}/include", "--target=riscv64-unknown-elf"]  # {c_tree}, {module}, {stem}
kit_types = ["list_node"]            # fields belong to the holder
value_types = ["id_t"]
lock_types = ["spin"]
lock_type_words = ["spinlock"]
percpu_globals = ["current_ctx"]
percpu_structs = []
membership_witness_macros = []
base_type_min_containers = 2

[c.argument_rewrites]                # full-match pattern = replacement ("" drops the argument)
"-march=.*" = "-march=rv64imac"

[c.calls]                            # callee-name patterns
lock = "lock"
unlock = "unlock"
alloc = "alloc"
free = "free"
free_object_after_first = "cache"    # these frees take the cache first, the object second

[c.membership]                       # insertion calls: element first, container last
list = ["list_insert"]
hash = ["hash_insert"]

[c.cleanup]                          # a local of this type calls the destructor at scope end
class_type = "^class_(.+)_t$"
destructor = "class_{name}_destructor"

[image]
arch = "riscv64"
thread_pointer = "current_ctx"
percpu_sections = []
[image.symbol_classes]               # data symbols that are not state
compiler_symbol = "^__key$"

[contract_graph]
function_prefix = "Prog/"            # a function node <prefix><dir>/<file>/<Fn> is <dir>/<file>.c

[topics]
prefix = "notifications.example."
```

`tests/fixtures/mini/cgraph.toml` is a complete example.
