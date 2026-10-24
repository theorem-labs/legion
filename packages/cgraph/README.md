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
  the addresses of globals a global's initializer stores (`global_initializers`), and list or hash
  insertions. A field reached through `->` or `*` belongs to the struct it points to; anonymous
  members, also tagged ones, are flattened. A field access whose address is taken (`&p->m`, an array
  member that decays) is marked `address`.
- **What the compiler removes is left out**, and kept with a reason: `sizeof`/`_Alignof` operands,
  `typeof` and cast types, const scalars, the unselected associations of `_Generic` (their types are
  read from the macro bodies that write them, or, where a macro pastes them together, the one
  association with the selection's own type is the selected one; a call of the function a `_Generic`
  selects is a direct call), branches whose condition folds to a constant (also through inline
  helpers that return one truth value and locals that keep their initializer; a weak definition
  decides nothing, because another unit may replace it), the cases a `switch` on a value clang folds
  does not reach (`switch (sizeof(x))`, `switch (_NSIG_WORDS)`), the right operand of a decided `&&` or
  `||`, values without calls or effects that are discarded (`(void)x`, `(x, 0)`, an expression
  statement; the last statement of `({ ... })` is its value), the operands of
  `__builtin_constant_p`, and statements after a `return`, a `goto`, a noreturn builtin or function,
  or a `do { ... } while (0)` that ends in one (`BUG()`). Branches of `__builtin_constant_p` and
  `__builtin_object_size`, also as the left operand of `&&` or `||`, are left to the machine code:
  their calls and global accesses stay only where the image has them.
- **Implied calls**: a local of a cleanup class (`c.cleanup.class_type`) calls its destructor with
  the constructor's arguments; a local with any other cleanup attribute calls the function it names
  (`c.cleanup.attribute_macros` maps macros such as `__free(x)` to that function, and a macro that is
  only another function's name, such as `#define __free_path_put path_put`, names that function).
- **The image** (ELF and `System.map`), decoded with capstone. Calls come from `jal`/`j` and from
  `auipc`+`jalr`, named by the code symbol at the target (`image.call_aliases` maps a library
  call the compiler substitutes, such as `bcmp`, to the one C names). An assembly routine, which the
  symbol table gives a size but no function type, is code like any other, so it takes its address
  from the zero-size marker that often sits at its first instruction. Data addresses come from
  `auipc` arithmetic, shared anchors and compressed loads included, and count only inside the
  declared size of an object symbol that `System.map` names. An address a register holds survives a
  spill to a frame slot and the reload from it. Where every path into a load gives its base a known
  address but not the same one, the function reads each of those objects on some path
  (`load_or_store_some_path`); where any path leaves the base unknown, no object is claimed. A call
  to a function whose code has no return and no jump out of itself (`rest_init`, `panic`) does not
  continue, so code the compiler places after it is reached only by its other paths. Nor does an
  `ebreak` that `[image.trap_table]` lists without the resume flag (a BUG); a WARN's resumes, as does
  any trap the table does not list.
  Thread-pointer accesses are per-CPU state. Globals the machine code addresses but the C does not
  name become nodes with image-only access edges.
- **What the image cannot tell apart is named by every name it has.** An address one past the end of
  an object is that object's end pointer and, where another object starts there, that object too;
  both names are kept, and a C access to either matches. Where the symbol table has a symbol of its
  own at the address that `System.map` leaves out (a jump table, a constant pool), the address is
  that symbol's and the object's end pointer is not claimed. The start of a block of globals LLVM
  merged (`.L_MergedGlobals`) is also its first member's address: forming it accesses no member, so
  it is recorded in `exclusions.json` (`image_access` / `merged_globals_block_base`, its `detail`
  holding the block's size and the names its start has) and each member is named where a
  displacement from the block reaches it.
- **Field offsets** come from every offset from a base the machine code forms, not only from the
  ones it dereferences: the displacement of a load or store, the immediate of an `addi`, the constant
  a register already carries (with the shift that forms the size of an array element), and the sum of
  the two where a load or store splits one offset over both.
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
caller's machine code also makes are the caller's too (the helper is inlined at another site). A
header's static function that several units define takes the facts of the unit whose image copy it
is (its ELF `FILE` symbol).

These rules follow what the machine code shows, each only where it shows it:

- A function whose address the C passes (a callback, a deferred free) is folded into the caller where
  the caller's machine code neither calls it nor forms its address, and makes a call or addresses a
  global that only that function explains, not the caller's own C. Where the image forms the address
  and also inlined a copy (an action a helper calls on its error path), the calls and globals only
  the copy explains are the caller's, marked `partial_inline`.
- A static function's parameter that every call site of its unit gives one global (directly, or
  through a static caller's parameter, which the facts may also type as its struct), in a function
  whose address the unit does not take, is that global where the machine code addresses it (or an
  address its initializer stores): `analysis.specialized_parameters`.
- A folded helper's field access through its parameter is rebased to the caller only where the
  argument's `displacement` is given (0 for the caller's pointer itself, the member's offset for
  `wb_exit(&bdi->wb)`): the displacement is added, the argument's own parameter kept, and the access's
  own displacement stays as `inner_displacement`, for code that keeps the member's address in a
  register. A pointer loaded through the parameter (`f(path->dentry)`) is another object and is not
  rebased. A parameter is followed to its callers' globals (above) on the same rule.
- A read of a global whose file-scope initializer stores other globals' addresses reaches each of
  those globals the machine code forms (the compiler folded the field's value); the edge names the
  holder in `initializer`.
- A call through a slot of a table that the function's own unit defines is a `call_indirect` edge
  (`resolution: table_slot`) where the image calls the initializer's target. A contract graph's slot
  edge needs that table's initializer; another unit's table of the same name does not count.

## Nodes and ids

Ids follow linkage, never a collision: `function:<name>` and `global:<name>` for external linkage,
`function:<name>@<file>` and `global:<name>@<file>` for a static (a function-local static is
`global:<function>.<name>@<file>`), `struct:<name>` for a header's struct and `struct:<name>@<file>`
for one a C file defines, `percpu:` for per-CPU state, `subsystem:<name>`. A definition that appears
later never renames an existing node. Of a weak and a strong definition of one name, the strong one is
the node and the weak one is listed in its `weak_definitions`. Weakness is read from the declaration's
attributes, so a definition whose text begins inside a macro (`notrace ... __weak sched_clock`) or
that a macro generates is recognized.

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
addresses (a C global at the start of a merged-globals block the function's machine code forms
counts as addressed: `analysis.image.block_start_names`), embeddings against clang's layout, and
field accesses against the machine code. A field
matches at its displacement from the base register: the sum of offsets along the member chain,
through constant subscripts, through a local every definition of which points the same way
(`d = &desc->irq_data`) and through an inline helper that returns such an address, less the member
offset a `container_of` subtracts (negative included). Past a variable index the facts also give the
displacement from the indexed element (`element_displacement`), and a global member's own offset
stands for code that keeps the member's address in a register; both are tried after the sum. A field
access through a parameter names it (`param`), and so does a call argument that is a member's address
(`displacement`), so a folded callee's offsets rebase onto its caller's. It matches as a load or
store displacement, an `addi` constant or its negative, a global's address offset, a narrower access
inside the field, a bitfield's storage unit, the base itself for a member whose address is taken at
offset 0 (`member_address_at_zero`), or an address inside a global the function's C names: the
field's own offset in its struct at a place that struct has there (`global_object_field`, or
`global_object_narrower_access` inside the field's bytes). Link gives those places in
`analysis.global_struct_offsets` from clang's layouts: the global's own struct, each struct it embeds
at any depth, and each element of an embedded array or of the global's own array (counted from the
image symbol's size). Accesses to objects on the stack or in registers, and functions whose machine
code makes no memory access, are counted apart.

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
[image.trap_table]                   # optional: which ebreak traps resume (the kernel's __bug_table)
section = "__bug_table"              # one entry per trap; a missing section stops the run (exit 3)
entry_size = 8
address_offset = 0                  # the trap's address: absolute, or with relative = true a signed
address_size = 4                    #   offset from the address field itself
relative = true
flags_offset = 4
flags_size = 2
resume_flags = 1                    # a trap whose flags share a bit resumes (WARN); one listed
                                     #   without it stops there (BUG), and the flow ends after it

[contract_graph]
function_prefix = "Prog/"            # a function node <prefix><dir>/<file>/<Fn> is <dir>/<file>.c

[topics]
prefix = "notifications.example."
```

`tests/fixtures/mini/cgraph.toml` is a complete example.
