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
  read from the macro bodies that write them, a typedef name standing for its type, or, where a
  macro pastes them together, the one association with the selection's own type is the selected
  one; a call of the function a `_Generic` selects is a direct call), branches whose condition
  folds to a constant (also through inline helpers whose reachable returns give one value or one
  truth value, a return after one the helper always takes being unreachable, a helper's value for
  the arguments its call passes, and locals every store of which gives one value; a weak definition
  decides nothing, because another unit may replace it),
  the cases a `switch` on a value clang folds does not reach (`switch (sizeof(x))`,
  `switch (_NSIG_WORDS)`), the right operand of a decided `&&` or `||`, an operand of `&` or `*`
  whose other operand is 0 (`IS_DAX(inode)` without `CONFIG_FS_DAX`), the left operand of an `&&`
  whose right one is 0 or of an `||` whose right one is 1 (`!pgdat || !numa_demotion_enabled`
  without NUMA), discarded values (`(void)x`, `(x, 0)`, an expression statement such as
  `get_user_ns(ns);`, also as an `if`'s branch or a loop's body, an `if` whose branches have no
  effect such as `if (dev->parent) pm_runtime_get_sync(dev->parent)` without `CONFIG_PM`, stores to a
  local or a parameter that compiled code never reads, such as `local_lock`'s `__tl`, and arguments
  to a parameter the callee never reads in compiled code), of which only the parts
  with an effect are code (`psi_sched_switch(..., !task_on_rq_queued(prev) || p->x)` keeps the
  `READ_ONCE` and drops the read of `p->x`; the last statement of `({ ... })` is its value).
  A parameter is ignored transitively (`from_kuid_munged` passes its namespace only to
  `from_kuid`, which ignores it), whatever its type (an array parameter is a pointer, a struct
  passed by value need not be copied), and so are the arguments past the named parameters of a
  variadic function that has no `va_start` (`sysfs_emit` without `CONFIG_SYSFS`); for a callee that
  is not inline the image decides, and so it does for the value of a local only such an argument
  reads (`free_area_init_core`'s `nid`). Also left out: the operands of `__builtin_constant_p`,
  symbols named only in `typeof`, and statements after a `return`, a `goto`, a noreturn builtin or
  function, or a `do { ... } while (0)` that ends in one (`BUG()`); a label is code only when
  control falls into it or a goto in code targets it. A function has no effect when its compiled
  code, as the rules above leave it, has none: no asm other than one without `volatile` that has an
  output (`RELOC_HIDE` under `this_cpu_ptr`), no volatile access, no store except to its own locals
  (also by `__builtin_mul_overflow` and its kind), no loop that may not end (a constant controlling
  expression, read from the parts libclang gives, or from the header as written where it leaves
  some out; C11 lets the compiler assume any other loop ends), no cleanup with an effect, and
  calls only to such functions. So the `WARN_ON_ONCE` under `IS_ENABLED(CONFIG_DEBUG_ATOMIC)` in
  `instrument_atomic_check_alignment` is no effect. Calls are followed 16 deep; a call past that, or
  back into a function whose analysis is under way, has an effect and reads its named arguments.
  Branches of `__builtin_constant_p` and `__builtin_object_size`, also as the left operand of `&&`
  or `||`, are left to the machine code: their calls, global accesses and field accesses (marked
  `conditional`) stay only where the image has them.
- **Implied calls**: a local of a cleanup class (`c.cleanup.class_type`) calls its destructor, with
  the local's address where the class is a struct (a lock guard's also standing for the lock its
  constructor took) and with the constructor's arguments otherwise; a local with any other
  cleanup attribute calls the function it names (`c.cleanup.attribute_macros` maps macros such as
  `__free(x)` to that function, and a macro that is only another function's name, such as
  `#define __free_path_put path_put`, names that function). A declaration of several cleanup locals
  (`scoped_seqlock_read`'s state and context pointer) names one function per declarator, in order.
  A lock or free action through a helper that returns its argument (`spinlock_check(sst->lock)`)
  names the member that argument is, as `kfree(p->buf)` does.
- **The image** (ELF and `System.map`), decoded with capstone. Calls come from `jal`/`j` and from
  `auipc`+`jalr`, named by the code symbol at the target (`image.call_aliases` maps a library
  call the compiler substitutes, such as `bcmp`, to the one C names). An assembly routine, which the
  symbol table gives a size but no function type, is code like any other, so it takes its address
  from the zero-size marker that often sits at its first instruction. Data addresses come from
  `auipc` arithmetic, shared anchors and compressed loads included, and count only inside the
  declared size of an object symbol that `System.map` names. An address a register holds, an
  unknown base plus a constant, or a known address plus an index survives a spill to a frame slot
  and the reload from it, until a store over any byte of the slot, a store through the other frame
  register, or a move of the frame register; a field reached from the reloaded register keeps its
  whole offset (`pgdat + 3752 + 352`). Where every path into a load gives its base a known address
  but not the same one, the function reads each of those objects on some path
  (`load_or_store_some_path`); where any path leaves the base unknown, no object is claimed. A
  loop's first pass is a path too: a load or store whose base the loop changes reads the
  address the loop enters with. An address formed with an `addi`, or an index added to a known
  address, is an access of its own only where the code uses it other than as the base of its loads
  and stores (passes, returns, stores or compares it); an index added for a table the next load
  reads is that load's access. An index never takes an access below the object it starts from
  (`buf[len - 1]`). A call to a function whose code has no return and no jump out of itself
  (`rest_init`, `panic`) does not continue, so code the compiler places after it is reached only
  by its other paths. Nor does an `ebreak` that `[image.trap_table]` lists without the resume flag
  (a BUG); a WARN's resumes, as does any trap the table does not list.
  Thread-pointer accesses are per-CPU state. Globals the machine code addresses but the C does not
  name become nodes with image-only access edges.
- **What the image cannot tell apart is named by every name it has.** A loop's bound, an address
  the code only compares or subtracts, may be the end of an object: in C two pointers compared point
  into one object, so the bound names the object its other side points into, and an address one
  past an object is that object's end pointer and, where another object starts there, that object
  too. No load or store reads the end of an object, and an address passed or returned is no bound
  (a string right after an object has the object's end address). Where the symbol table has a
  symbol of its own at the address that `System.map` leaves out (a jump table, a constant pool),
  the address is that symbol's and the object's end pointer is not claimed. The start of a block of
  globals LLVM merged (`.L_MergedGlobals`) is also its first member's address: forming it accesses
  no member, so it is recorded in `exclusions.json` (`image_access` / `merged_globals_block_base`,
  its `detail` holding the block's size and the names its start has) and each member is named where
  a displacement from the block reaches it. Zero-size markers at a function's first instruction
  (`_sinittext`, `__sched_text_start`) name the function's address as well: the function's address
  is recorded, and the markers are an `image_access` / `code_marker_at_function_start` item whose
  `detail.names` a C access to one of them matches.
- **Field offsets** come from every offset from a base the machine code forms, not only from the
  ones it dereferences: the displacement of a load or store, the immediate of an `addi`, the constant
  a register already carries (with the shift that forms the size of an array element; on a loop's
  first pass, the constant a register was biased by before the loop, as `addi s1, a0, 0x40`), and the
  sum of the two where a load or store splits one offset over both. A displacement from a linker
  table's start symbol (`__start_x`, zero-size at the table's first object) counts for that name. The
  frame is kept apart, and adds no field offset: each load and store through sp, through s0 where the
  prologue sets it from sp, or through a register holding a frame address (`frame_accesses`, without
  the prologue's saves of callee-saved registers and their restores), each frame address the code
  forms (`frame_addresses`), each it passes in an argument register to a call (`frame_arguments`),
  and each load or store past a variable index added to a frame address (`frame_indexed`, at the
  address's offset), as offsets from the value sp had on entry.
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
  `wb_exit(&bdi->wb)`, the address of the caller's local or global object): the displacement is added,
  the argument's own parameter kept, the argument's `base` taken (`local` and the `object` for
  `f(&ac)`, `container_of` for `f(container_of(ref, ...))`, whose negative displacement the image
  addresses), and the access's own displacement stays as `inner_displacement`, for code that keeps
  the member's address in a register. An address that steps by a helper's parameter
  (`sb->s_writers.rw_sem + level - 1`) gives an `index` term: a caller's constant argument adds its
  steps at the fold, the caller's own parameter carries the term on. A pointer loaded through the
  parameter (`f(path->dentry)`) is another object and is not rebased. A parameter is followed to its
  callers' globals (above) on the same rule. `analysis.object_arguments` lists each local object whose
  address a function passes to a call.
- A local's or parameter's address is derived from the values that reach each use: the assignments
  before it (or after it, around a loop or a backward goto), less those an assignment on every path
  overwrites (also a `for` header's: the condition's `(p = next_task(p)) != &init_task` in the body,
  the init in the whole loop), without null pointers (`for (; se; se = NULL)`, a helper that returns
  NULL), through `?:`, `({ ... })`, `_Generic`, pointer arithmetic, an array's decay, and the return
  of any helper of the unit. A helper not declared inline, one that returns NULL or an address (a
  select the compiler keeps), and a `c ? container_of(...) : NULL` nothing tests also give
  `inner_displacement`, from the register that holds the result.
- The evaluator folds what the compiler does: a helper whose every live return gives one value (a
  return under a constant-false test is no return), a file-scope static nothing in the unit writes
  or takes the address of (its initializer), `x && 0` / `x || 1` whatever `x` is, and `(x, 0)`. GNU's
  `a ?: b` is no constant. Where one value is not enough it takes every value each part may hold
  (`expressions.values`, up to 16): a helper's call is what the helper returns for the arguments
  passed, its returns under the `if` and `switch` conditions that reach each (its template on its
  parameters; dma_go_direct(dev, mask, NULL) is true, softleaf_type returns none of the device
  types without `CONFIG_DEVICE_PRIVATE`), and a local is any value stored in it, `|=`, `&=` and `^=`
  included; the expression is decided where every combination gives one value or one truth value.
- A read of a global whose file-scope initializer stores other globals' addresses reaches each of
  those globals the machine code forms (the compiler folded the field's value); the edge names the
  holder in `initializer`.
- A call through a slot of a table that the function's own unit defines is a `call_indirect` edge
  (`resolution: table_slot`) where the image calls the initializer's target. A contract graph's slot
  edge needs that table's initializer; another unit's table of the same name does not count.
- A folded helper's code that its caller's arguments decide against is dead for that caller: the facts
  give each function a table of `guards`, conditions on its parameters (if, `?:`, the right operand of
  `&&`/`||`, switch cases, the statements after `if (c) return;`, and the code past a label that only
  gotos before it reach, under the guards of those gotos; none across a label a later goto reaches),
  and each event the guards it is compiled under. A guard states each variable as the values it may
  hold: a parameter nothing writes (or whose address escapes only later in the text, outside any loop)
  is the caller's argument, a variable that plain assignments write is a choice among its first
  value and each value assigned, one that `|=`, `&=` or `^=` change also each value those make of
  it, and a call of a helper of the unit is its template with the arguments put in
  (`node_online(this_node)` is `this_node == 0` without NUMA). A guard is decided where every value
  it may take agrees, and an `==` or `!=` between two reads of one value is decided whatever that
  value is: a pointer local set once (by its initializer or one assignment no loop or label repeats)
  whose value the facts cannot state is named `{"v": name}` (switch_hrtimer_base's new_cpu_base is
  this_cpu_base whichever store set it). A call argument gives the value it passes (`expr`: a
  constant, a choice, a named value, or an expression on the caller's own parameters, at each
  type's width). At a
  fold, link puts the arguments in; a call or an access a guard decides against becomes `unevaluated`
  with reason `constant_argument` (an exclusion), other events are dropped, and the guards still open
  carry up to the next fold.
- A helper folded under a `__builtin_constant_p` choice marks all its events `conditional`, field
  accesses too: the machine code decides whether that branch was compiled, so a conditional field
  access with no match is excluded as `image_decided` (below).
- A field of a local object is held against the frame (`frames.py`). The object's slot B is where an
  image call passes the frame address B + d in the argument register where the C passes
  `&object + d` (`analysis.object_arguments` against `frame_arguments`; a struct local's cleanup
  gets its address too, and a lock guard's destructor the guard's: the local its inline constructor
  returns by value is the guard). Where a static callee's image copy dropped leading parameters
  (stop_machine_cpuslocked's `queue_stop_cpus_work(mask, fn, &msdata, &done)` takes `&msdata` in a1),
  the register is the C position less the one shift (up to 2) at which every object the C passes to
  that callee has a frame argument. The fields are then matched at B plus their displacement
  (`frame_slot_access`, a wider or narrower access over it, or, for an array member at a variable
  index, `frame_slot_indexed_access` or a formed address there). The frame alone decides no slot: on
  the kernel image (part `all`), a formed frame address where every accessed field is loaded or stored
  fits the fields of another function's object of another struct 21% of the time with two fields, 10%
  with three and 8% with four, so the 1,506 objects no call places are excluded as
  `local_object_without_frame_slot`. Of the 591 objects a call places, the 295 whose function passes
  other frame addresses too hold 847 fields at their slot and 372 at the best other frame argument.
- Every field access no displacement can show is an exclusion of kind `field_access`, the access
  itself as its detail, and the structure check counts each reason as often as it is recorded:
  `local_object_without_frame_slot`, `zero_size_member` (an empty struct member, as `struct
  pin_cookie` without lockdep), `no_memory_access_in_image`, `other_object`, `displacement_unknown`
  and `image_decided`. `field_offsets.frame_decoys` measures how often a slot holds fields by chance:
  the fields another call's frame argument would hold, and how often the frame alone would fit
  another struct. A miss at a frame slot (`no_access_at_frame_slot`) gives its `frame_slot`, the
  field's `displacement` from it, its `size` and the `object`, for a per-item rule.

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
addresses (a C global at the start of a merged-globals block the function's machine code forms, or a
zero-size marker at the start of a function it addresses, counts as addressed:
`analysis.image.block_start_names`), embeddings against clang's layout, and
field accesses against the machine code. A field
matches at its displacement from the base register: the sum of offsets along the member chain,
through constant subscripts, through a local every definition of which points the same way
(`d = &desc->irq_data`) and through an inline helper that returns such an address, less the member
offset a `container_of` subtracts (negative included). Past a variable index the facts also give the
displacement from the indexed element (`element_displacement`), and a global member's own offset (or
a `container_of` member's own offset, where the code holds the container itself) stands for code that
keeps that address in a register; both are tried after the sum. A field
access through a parameter names it (`param`), and so does a call argument that is a member's address
(`displacement`), so a folded callee's offsets rebase onto its caller's. It matches as a load or
store displacement, an `addi` constant or its negative, a global's address offset, a narrower access
inside the field, a bitfield's storage unit, the base itself for a member whose address is taken at
offset 0 (`member_address_at_zero`), an access inside the first element of an array member whose
address the code takes (`info = same->info`; the facts give `element_size`:
`array_member_first_element`), or an address inside a global the function's C names: the
field's own offset in its struct at a place that struct has there (`global_object_field`, or
`global_object_narrower_access` inside the field's bytes). Link gives those places in
`analysis.global_struct_offsets` from clang's layouts: the global's own struct, each struct it embeds
at any depth, and each element of an embedded array or of the global's own array (counted from the
image symbol's size). A field reached through a pointer that walks an array also matches one element
ahead (`next_element_displacement`: its offset less the struct's size, where the function steps a
register by that size, so the code keeps `i + 1`). Accesses to objects on the stack or in registers,
and functions whose machine code makes no memory access, are counted apart.

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
