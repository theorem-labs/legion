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
  allocate, free), direct calls, calls through function-pointer fields, the functions a function uses
  as values (passes, stores, compares or names in an initializer), callback tables, record layouts,
  and list or hash insertions. A field reached through `->` or `*` belongs to the struct it points
  to; anonymous members are flattened.
- **What the compiler removes is left out**, and kept with a reason: `sizeof`/`_Alignof` operands,
  `typeof` and cast types, const scalars, the unselected associations of `_Generic` (their types are
  read from the macro bodies that write them, or, where a macro pastes them together, the one
  association with the selection's own type is the selected one; a call of the function a `_Generic`
  selects is a direct call), branches whose condition folds to a constant (also through inline
  helpers that return one truth value and locals that keep their initializer), the right operand of
  a decided `&&` or `||`, and statements after a `return`, `goto` or noreturn builtin. Branches of
  `__builtin_constant_p` and `__builtin_object_size` are left to the machine code: their calls and
  global accesses stay only where the image has them.
- **Implied calls**: a local of a cleanup class (`c.cleanup.class_type`) calls its destructor with
  the constructor's arguments; a local with any other cleanup attribute calls the function it names
  (`c.cleanup.attribute_macros` maps macros such as `__free(x)` to that function).
- **The image** (ELF and `System.map`), decoded with capstone. Calls come from `jal`/`j` and from
  `auipc`+`jalr`, named by the ELF function symbol at the target (`image.call_aliases` maps a library
  call the compiler substitutes, such as `bcmp`, to the one C names). Data addresses come from
  `auipc` arithmetic, shared anchors and compressed loads included, and count only inside the
  declared size of an object symbol that `System.map` names. Thread-pointer accesses are per-CPU
  state. Globals the machine code addresses but the C does not name become nodes with image-only
  access edges.
- **The contract graph** (optional): the proof's own graph. Its function nodes are joined to the graph's
  functions by image address (below); it confirms callback tables a C initializer also names, and
  gives the interfaces the measures map onto groups.

A unit's own functions are those its file or a C file it includes defines, and those with external
linkage that a header it includes defines (a fragment only that unit includes, as `kernel/rcu/tree.c`
includes `tree_stall.h`). Each is a function of the part when the image holds it. A header's helper is
a function of the part when the unit compiled a copy of it: the image calls it, or the unit takes its
address (a table of operations, a stored hook, a pointer in a file-scope initializer) and nothing
calls it. A helper is folded into a caller where the caller's machine code does not call it, so the
C layer sees what the compiler inlined. A helper the image calls stays a call; its calls that the
caller's machine code also makes are the caller's too (the helper is inlined at another site).

## Nodes and ids

Ids follow linkage, never a collision: `function:<name>` and `global:<name>` for external linkage,
`function:<name>@<file>` and `global:<name>@<file>` for a static (a function-local static is
`global:<function>.<name>@<file>`), `struct:<name>` for a header's struct and `struct:<name>@<file>`
for one a C file defines, `percpu:` for per-CPU state, `subsystem:<name>`. A definition that appears
later never renames an existing node. Of a weak and a strong definition of one name, the strong one is
the node and the weak one is listed in its `weak_definitions`; a weak definition a macro generates is
recognized through the macros it expands.

Embedding and list or hash membership join data structures into groups. A value type (no pointer at
any depth, or listed in `c.value_types`) and a base that at least `c.base_type_min_containers` types
embed do not join. A group of two or more is a subsystem, named from its largest struct by clang's
size.

## Owners and topics

`owners.json` lists one owner per function of the part, per data structure that a part function
accesses or a structure link joins, per per-CPU state, and per subsystem. The owner's key is the node
id. `contract` is the contract graph's function node whose `addr` is an image address of the function
(one of its `image_addresses`), `null` when there is none, or a list of node ids when several nodes
give addresses of its image copies (a header helper compiled into several units). The address is the
machine code the contract is about, so the join holds where the node's id names another C file than
the function's own: a file name with a hyphen, a function of another C file or of a header, an alias
symbol. Each contract node belongs to exactly one owner. Every other function node is in
`exclusions.json` as `contract_function` with a reason: `outside_function_prefix` (another
program's node), `no_image_symbol` (no address: a macro, an always-inline helper or an assembly
label), `no_part_function_at_address` (assembly, or a function of a unit outside the part; the detail
names the image symbol there) or `address_of_several_part_functions`. Two nodes that give one
address fail the hard check `contract_mapping_unique`. `topics.json` gives each owner's subscriptions
under `topics.prefix`: `node.<id>.needs` and `all` for everyone; `node.<callee>.callers`,
`node.<caller>.callees` and `node.<data>.users` for a function; `node.<linked>.linked` for a data
structure or a subsystem; `node.<neighbour>.neighbors` for every neighbour in the three layers. In a
topic, the id has every character outside `[A-Za-z0-9_:-]` percent-encoded, so it stays one token.

## Checks

Each run writes `checks.json`: C calls against image calls (disagreements classified: a callback the
compiler devirtualized, a caller with a weak alternate, a tail call), state accesses against image
addresses, embeddings against clang's layout, and field accesses against the machine code. A field
matches at its displacement from the base register (the sum of offsets along a `.` chain, less the
member offset a `container_of` subtracts), as a load or store displacement, an `addi` constant or its
negative, a global's address offset, a narrower access inside the field, or a bitfield's storage
unit; accesses to objects on the stack or in registers, and functions whose machine code makes no
memory access, are counted apart.

The hard checks fail the run (exit 1), each with a test that makes it fail alone: a parse error; a
function of the part's files that the image holds but the graph lacks; a global the part's code
addresses but the graph lacks; a node or edge without provenance; an embedding without a layout; a
membership without a C site; an invalid or repeated id; a node that must have one owner (read from
the graph) without exactly one; an owner without topics or with an invalid one; an absolute path in
any output; and call agreement (|both| / |either|), state access agreement, and field-offset matches
(over all candidates and over the first 50) below the thresholds of `[checks]` or of the part's own
`checks` table. Set the thresholds just below what a reviewed build measured, so a regression fails.

## Incremental builds

With `--cache`, each unit's facts are kept under a key of the tool's code, the vocabulary, the unit's
arguments and the contents of every file it included. A later build parses only the units whose key
changed, so after a landing it parses the changed files (and every unit including a changed header)
and gives the same bytes as a full build.

The same directory keeps each image function's decoded facts under `image/`. An entry's key covers
the image, `System.map`, the `[image]` table, the decoder's source and the capstone and pyelftools
versions, the function's symbol, its own result registers, and the parameter count of each function
it calls or jumps to directly: the only C facts its decoding reads. A change to another function's
signature leaves the entry in use. An entry that cannot be read or is malformed stops the run with
exit status 3 and names the cache directory to remove.

It keeps two more layers, each reused only while every input it reads is unchanged:

- `link/<part>/<key>.json`, the link's core, one per part (a new core replaces the part's earlier one):
  the nodes, edges, exclusions and analysis that the unit facts, the image and the configuration
  decide. Its key covers the package's code, the configuration's rules, the part's files, each unit's
  facts key, the image's and `System.map`'s SHA-256, and the image copy the contract graph's addresses
  pin. The contract layer (each function's contract, the callback-table edges, the contract functions
  of the part) is linked again on every build, so a landing that changes only the contract graph reuses
  the core and gives the same bytes as a full link.
- `interfaces/<sha256>.json`, the top-level lines and identifier runs of each Lean module an interface
  is declared in, keyed by the module's contents and the rules that read it, so only a changed module
  is read into tokens again. A declaration that cannot be read is an input error (exit 3).

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
checks = { min_call_agreement = 0.98 }  # this part's thresholds, over [checks]

[checks]                             # the least agreement a run must reach, each from 0 to 1
min_call_agreement = 0.9
min_access_agreement = 0.7
min_field_offsets = 0.85

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
allocate = "alloc"
free = "free"
free_object_after_first = "cache"    # these frees take the cache first, the object second

[c.membership]                       # insertion calls: element first, container last
list = ["list_insert"]
hash = ["hash_insert"]

[c.cleanup]                          # a local of this type calls the destructor at scope end
class_type = "^class_(.+)_t$"
destructor = "class_{name}_destructor"
attribute_macros = { "__free" = "__free_{arg}" }  # a macro that puts a cleanup attribute on a local

[image]
arch = "riscv64"
thread_pointer = "current_ctx"
percpu_sections = []
call_aliases = { bcmp = "memcmp" }   # an image callee that stands for the C function named
[image.symbol_classes]               # data symbols that are not state
compiler_symbol = "^__key$"

[contract_graph]
function_prefix = "Prog/"            # a function node <prefix><dir>/<file>/<Fn> is <dir>/<file>.c

[topics]
prefix = "notifications.example."
```

`tests/fixtures/mini/cgraph.toml` is a complete example.
