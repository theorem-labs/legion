"""The C facts' guards: the conditions on a function's parameters under which each event is compiled,
and the values call arguments pass, as link reads them at a fold (link._bind)."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from conftest import FIXTURE

from cgraph import clangfacts, commands, config, expressions, link

CONFIG = config.load(FIXTURE / "cgraph.toml")

PROLOGUE = """
typedef unsigned int gfp_t;
struct dev { int x; struct dev *next; };
void warn(void); void note(void); void other(void); void sw0(void); void sw1(void); void dflt(void);
void pr(void); void late(void); void kept(void); void lab(void); void err(void); void neg(void);
"""


def facts_of(tmp_path: Path, code: str) -> dict[str, Any]:
    tree = tmp_path / "tree"
    (tree / "src").mkdir(parents=True, exist_ok=True)
    (tree / "src" / "g.c").write_text(PROLOGUE + code)
    arguments = commands.template_arguments(CONFIG.flags, tree, "src/g.c")
    facts: dict[str, Any] = clangfacts.extract("src/g.c", tree, arguments, CONFIG.vocabulary)
    assert facts["errors"] == [], facts["errors"]
    return facts


def constant(value: int) -> dict[str, Any]:
    return {"unit": None, "param": None, "expr": {"c": value, "w": 32, "s": True}}


def unknown() -> dict[str, Any]:
    return {"unit": None, "param": None}


def compiled(facts: dict[str, Any], function: str, arguments: list[dict[str, Any]]) -> set[str]:
    """The callees of the function's calls that the arguments do not decide against."""
    record = facts["functions"][function]
    table = record.get("guards", [])
    live = set()
    for call in record["events"]["calls"]:
        guards = [table[index] for index in call.get("guards", [])]
        if link._decided(guards, arguments) is not None:
            live.add(call["callee"])
    return live


def test_branches_returns_switch_cases_and_short_circuits_guard_on_the_parameters(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline void change(struct dev *dev, int state)
        {
            if (state == 3)
                return;
            warn();
            if (state && dev->x)
                note();
            switch (state) {
            case 0: sw0(); break;
            case 1: case 2: sw1(); break;
            default: dflt();
            }
            state > 5 ? pr() : other();
        }
        void root(struct dev *d) { change(d, 3); }
        """,
    )
    assert compiled(facts, "change", [unknown(), constant(3)]) == set()
    assert compiled(facts, "change", [unknown(), constant(0)]) == {"warn", "sw0", "other"}
    assert compiled(facts, "change", [unknown(), constant(2)]) == {"warn", "note", "sw1", "other"}
    assert compiled(facts, "change", [unknown(), constant(9)]) == {"warn", "note", "dflt", "pr"}
    assert compiled(facts, "change", [unknown(), unknown()]) == {
        "warn", "note", "sw0", "sw1", "dflt", "pr", "other",
    }  # fmt: skip
    (call,) = facts["functions"]["root"]["events"]["calls"]
    assert call["arguments"][1]["expr"] == {"c": 3, "w": 32, "s": True}


def test_a_local_that_keeps_its_initializer_stands_for_it(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline void offline(int node, gfp_t gfp_mask)
        {
            gfp_t warn_gfp = gfp_mask & (4u | 2u);
            if (warn_gfp != (4u | 2u))
                return;
            pr();
        }
        """,
    )
    assert compiled(facts, "offline", [unknown(), constant(0x400)]) == set()
    assert compiled(facts, "offline", [unknown(), constant(0x406)]) == {"pr"}


def test_a_parameter_the_function_writes_holds_the_argument_or_what_it_writes(tmp_path: Path) -> None:
    """With 4 the parameter holds 4 or 0, neither 5; with -1 it may hold 0."""
    facts = facts_of(
        tmp_path,
        """
        static inline void alloc(int nid)
        {
            if (nid == -1)
                nid = 0;
            if (nid == 5)
                late();
            if (nid == 0)
                other();
            kept();
        }
        """,
    )
    assert compiled(facts, "alloc", [constant(4)]) == {"other", "kept"}
    assert compiled(facts, "alloc", [constant(5)]) == {"late", "other", "kept"}
    assert compiled(facts, "alloc", [constant(-1)]) == {"other", "kept"}


def test_a_parameter_is_read_at_its_own_width(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline void narrow(unsigned char f) { if (f & 0x100) late(); kept(); }
        """,
    )
    (call,) = [item for item in facts["functions"]["narrow"]["events"]["calls"] if item["callee"] == "late"]
    assert compiled(facts, "narrow", [constant(0x1FF)]) == {"kept"}
    assert call["guards"]


def test_no_guard_crosses_a_goto_label(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline int labels(int s, int t)
        {
            if (t)
                goto inner;
            if (s)
                goto out;
            late();
            if (s == 7) {
        inner:
                lab();
            }
        out:
            err();
            return 0;
        }
        """,
    )
    assert compiled(facts, "labels", [constant(1), constant(0)]) == {"lab", "err"}
    assert compiled(facts, "labels", [constant(0), constant(0)]) == {"late", "lab", "err"}


def test_call_arguments_say_the_values_they_pass(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        void take(void *p, int v, unsigned long w);
        void pass(int k) { take((void *)0, k + 1, 7); }
        """,
    )
    (call,) = facts["functions"]["pass"]["events"]["calls"]
    null, value, seven = call["arguments"]
    assert link._constant(null) == 0
    assert link._constant(seven) == 7
    assert expressions.evaluate(expressions.substitute(value["expr"], [constant(4)["expr"]])) == 5


def test_a_helper_that_always_returns_one_negative_value_compares_as_that_value(tmp_path: Path) -> None:
    """`helper() < 0` where the helper returns -95: true, not the truth value 1 compared."""
    facts = facts_of(
        tmp_path,
        """
        static inline int unsupported(void) { return -95; }
        static inline int never(void) { return 0; }
        void check(void) { if (unsupported() < 0) neg(); if (never() < 0) late(); kept(); }
        """,
    )
    calls = {call["callee"] for call in facts["functions"]["check"]["events"]["calls"]}
    dead = {item["call"] for item in facts["functions"]["check"]["events"]["unevaluated"] if "call" in item}
    assert calls >= {"neg", "kept"}
    assert "late" in dead and "neg" not in dead


def test_an_argument_that_is_the_address_of_a_global_s_element_names_the_global(tmp_path: Path) -> None:
    """precompute_cache_shard_ids passes &wq_pod_types[k] through a local; free_area_init_node passes
    NODE_DATA(nid), an inline helper that returns &contig_page_data."""
    facts = facts_of(
        tmp_path,
        """
        struct pod { int n; };
        struct node { int id; };
        struct data { int seq; struct node shadow; };
        static struct pod pods[3];
        static struct node contig;
        static struct data datas[2];
        static inline struct node *node_data(int nid) { return &contig; }
        void use_pod(struct pod *p); void use_node(struct node *n);
        void caller(int nid)
        {
            struct pod *smt = &pods[1];
            struct node *pgdat = node_data(nid);
            struct node *shadow = &datas[0].shadow;
            use_pod(smt);
            use_pod(&pods[2]);
            use_node(pgdat);
            use_node(shadow);
        }
        """,
    )
    calls = facts["functions"]["caller"]["events"]["calls"]
    units = [(call["callee"], call["arguments"][0]["unit"]) for call in calls if call["callee"].startswith("use_")]
    assert [(callee, unit["kind"], unit["name"]) for callee, unit in units] == [
        ("use_pod", "global", "pods"),
        ("use_pod", "global", "pods"),
        ("use_node", "global", "contig"),
        ("use_node", "global", "datas"),
    ]


def test_an_array_member_whose_address_is_taken_gives_its_element_size(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        struct item { long fd; long offset; long done; int status; int pad; };
        struct range { long a; long b; int count; int pad; struct item info[]; };
        void walk(struct range *same) { struct item *info = same->info; for (; info->fd; info++) info->offset = 1; }
        """,
    )
    fields = facts["functions"]["walk"]["events"]["field_accesses"]
    (info,) = [item for item in fields if item["field"] == "info"]
    assert (info["offset"], info.get("address"), info["element_size"]) == (24, True, 32)


def test_a_parameter_whose_address_is_taken_later_holds_the_argument_before(tmp_path: Path) -> None:
    """warn_if_node_offline passes &gfp_mask to pr_warn after the conditions that decide it."""
    facts = facts_of(
        tmp_path,
        """
        void show(const gfp_t *p);
        static inline void offline(int node, gfp_t gfp_mask)
        {
            gfp_t warn_gfp = gfp_mask & (4u | 2u);
            if (warn_gfp != (4u | 2u))
                return;
            pr();
            show(&gfp_mask);
            if (gfp_mask == 6u)
                late();
        }
        """,
    )
    assert compiled(facts, "offline", [unknown(), constant(0x400)]) == set()
    # After its address escapes the parameter may hold anything: `gfp_mask == 6u` stays open.
    assert compiled(facts, "offline", [unknown(), constant(0x406)]) == {"pr", "show", "late"}


def test_a_parameter_the_function_assigns_is_any_value_it_may_hold(tmp_path: Path) -> None:
    """__mutex_lock_common: `if (!use_ww_ctx) ww_ctx = NULL;`, then `if (ww_ctx)`. With a NULL
    ww_ctx both values are NULL; with another, the branch is open."""
    facts = facts_of(
        tmp_path,
        """
        static inline int common(struct dev *ctx, int use_ctx)
        {
            if (!use_ctx)
                ctx = (void *)0;
            if (ctx)
                warn();
            kept();
            return 0;
        }
        """,
    )
    assert compiled(facts, "common", [constant(0), constant(0)]) == {"kept"}
    assert compiled(facts, "common", [unknown(), constant(0)]) == {"warn", "kept"}


def test_code_past_a_label_only_forward_gotos_reach_is_under_their_guards(tmp_path: Path) -> None:
    """__devm_clk_get: `if (init) { ret = init(clk); if (ret) goto err_clk_init; } ... return clk;
    err_clk_init: clk_put(clk); err_clk_get: ...`; devm_clk_get passes NULL for init."""
    facts = facts_of(
        tmp_path,
        """
        int probe(void);
        static inline int get(int (*init)(void), int bad)
        {
            int ret = 0;
            if (bad)
                goto err_get;
            if (init) {
                ret = init();
                if (ret)
                    goto err_init;
            }
            kept();
            return 0;
        err_init:
            err();
        err_get:
            lab();
            return ret;
        }
        """,
    )
    null = {"unit": None, "param": None, "expr": {"c": 0, "w": 64, "s": False}}
    assert compiled(facts, "get", [null, unknown()]) == {"kept", "lab"}
    assert compiled(facts, "get", [unknown(), unknown()]) == {"kept", "err", "lab"}


def test_a_label_a_later_goto_reaches_gets_no_guard(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline void retry(int (*init)(void))
        {
            return;
        again:
            err();
            if (init)
                goto again;
        }
        """,
    )
    null = {"unit": None, "param": None, "expr": {"c": 0, "w": 64, "s": False}}
    assert compiled(facts, "retry", [null]) == set()


def test_an_expression_libclang_does_not_expose_is_no_value(tmp_path: Path) -> None:
    """errname: __errname(abs(err)), where abs is __builtin_choose_expr(…, ({ … }), …((void)0)).
    Its last child is the unselected (void)0, not the argument's value."""
    facts = facts_of(
        tmp_path,
        """
        const char *lookup(unsigned int err);
        static inline const char *name_of(unsigned int err) { if (err >= 512) late(); return 0; }
        const char *errname(int err)
        {
            return name_of(__builtin_choose_expr(1, ({ int x = err; x < 0 ? -x : x; }), ((void)0)));
        }
        """,
    )
    (call,) = [item for item in facts["functions"]["errname"]["events"]["calls"] if item["callee"] == "name_of"]
    assert "expr" not in call["arguments"][0]
    assert compiled(facts, "name_of", call["arguments"]) == {"late"}


def test_a_helper_s_returns_after_one_it_always_takes_are_not_its_value(tmp_path: Path) -> None:
    """kmem_cache_init → bootstrap_kmalloc_sheaves → bootstrap_cache_sheaves: with CONFIG_SLUB_TINY,
    calculate_sheaf_capacity returns 0 before any other return, so `if (!capacity) return;` ends
    bootstrap_cache_sheaves, and its barn_init (node_barn.nr_full) was never compiled."""
    facts = facts_of(
        tmp_path,
        """
        struct barn { int lock; int nr_full; };
        static inline unsigned int capacity_of(struct dev *d)
        {
            if (1 || d->x)
                return 0;
            return d->x * 2;
        }
        static inline void setup(struct dev *d, struct barn *b)
        {
            unsigned int capacity;
            capacity = capacity_of(d);
            if (!capacity)
                return;
            note();
            b->nr_full = 0;
        }
        """,
    )
    setup = facts["functions"]["setup"]["events"]
    assert "note" not in {call["callee"] for call in setup["calls"]}
    assert "note" in {item.get("call") for item in setup["unevaluated"]}
    assert not [item for item in setup["field_accesses"] if item["field"] == "nr_full"]


def test_a_local_every_store_gives_one_value_holds_it_and_one_with_two_values_does_not(tmp_path: Path) -> None:
    facts = facts_of(
        tmp_path,
        """
        static inline int one(void) { return 1; }
        void same(void) { int v; v = 0; if (v) late(); kept(); }
        void two(int k) { int v = 0; if (k) v = one(); if (v) late(); kept(); }
        void once(void) { static int done; if (done) return; done = 1; late(); }
        """,
    )
    assert "late" not in {call["callee"] for call in facts["functions"]["same"]["events"]["calls"]}
    assert "late" in {call["callee"] for call in facts["functions"]["two"]["events"]["calls"]}
    assert "late" in {call["callee"] for call in facts["functions"]["once"]["events"]["calls"]}


def test_the_evaluator_compares_at_each_operand_s_width_and_signedness(tmp_path: Path) -> None:
    """copy_thread: `ssp = 0; … ssp = shstk_alloc_thread_stack(p, args);` (a macro for 0), then
    `if (IS_ERR_VALUE(ssp)) return …;`. (unsigned long)0 >= (unsigned long)-4095 is false."""
    facts = facts_of(
        tmp_path,
        """
        int copy(int fn)
        {
            unsigned long ssp = 0;
            if (fn) {
                late();
            } else {
                ssp = 0;
                if ((unsigned long)(void *)(ssp) >= (unsigned long)-4095)
                    return -1;
                note();
            }
            kept();
            if ((unsigned char)511 == 255)
                other();
            return 0;
        }
        """,
    )
    calls = {call["callee"] for call in facts["functions"]["copy"]["events"]["calls"]}
    assert {"note", "kept", "other"} <= calls


FLAGS = """
static inline void update(struct dev *d, int flags)
{
    if (!d->x && (flags & 4))
        note();
    else if (flags & 8)
        late();
    kept();
}
"""


def test_an_argument_that_may_hold_several_values_decides_what_every_one_decides(tmp_path: Path) -> None:
    """attach_entity_cfs_rq: update_load_avg(cfs_rq, se, sched_feat(ATTACH_AGE_LOAD) ? 0 : SKIP_AGE_LOAD).
    Neither 0 nor 2 has DO_DETACH (8) or DO_ATTACH (4): both branches are dead."""
    facts = facts_of(tmp_path, FLAGS + "void attach(struct dev *d, int age) { update(d, age ? 0 : 2); }")
    (call,) = facts["functions"]["attach"]["events"]["calls"]
    assert compiled(facts, "update", call["arguments"]) == {"kept"}


def test_a_local_that_or_assignments_change_holds_each_value_they_may_give(tmp_path: Path) -> None:
    """dequeue_entity: `int action = UPDATE_TG; if (migrating) action |= DO_DETACH;` then
    update_load_avg(cfs_rq, se, action); faultin_page_range: `gup_flags = FOLL_TOUCH | …; if (write)
    gup_flags |= FOLL_WRITE;`. Neither value has DO_ATTACH; one has DO_DETACH."""
    facts = facts_of(
        tmp_path,
        FLAGS
        + """
        void dequeue(struct dev *d, int migrating) { int action = 1; if (migrating) action |= 8; update(d, action); }
        void fault(struct dev *d, int write) { int flags; flags = 1 | 2; if (write) flags |= 16; update(d, flags); }
        void other_write(struct dev *d, int k) { int flags = 1; flags += k; update(d, flags); }
        """,
    )
    calls = {name: facts["functions"][name]["events"]["calls"] for name in ("dequeue", "fault", "other_write")}
    arguments = {
        name: next(call for call in found if call["callee"] == "update")["arguments"] for name, found in calls.items()
    }
    assert compiled(facts, "update", arguments["dequeue"]) == {"late", "kept"}
    assert compiled(facts, "update", arguments["fault"]) == {"kept"}
    assert compiled(facts, "update", arguments["other_write"]) == {"note", "late", "kept"}


def test_a_helper_s_value_follows_the_arguments_its_call_passes(tmp_path: Path) -> None:
    """dma_alloc_attrs: `ops = get_dma_ops(dev)` is NULL without CONFIG_DMA_OPS, so
    dma_alloc_direct(dev, ops) → dma_go_direct(dev, mask, NULL) returns true: `ops->alloc` is dead.
    gpio_clk_driver_probe: devm_gpiod_get returns ERR_PTR(-ENOSYS) without CONFIG_GPIOLIB, so
    `if (IS_ERR(gpiod)) return …;` always returns."""
    facts = facts_of(
        tmp_path,
        """
        struct ops { int (*alloc)(void); };
        static inline const struct ops *get_ops(struct dev *d) { return (void *)0; }
        static inline int use_iommu(struct dev *d) { return 0; }
        static int go_direct(struct dev *d, unsigned long mask, const struct ops *ops)
        {
            if (use_iommu(d))
                return 0;
            if (__builtin_expect(!ops, 1))
                return 1;
            return 0;
        }
        static inline int alloc_direct(struct dev *d, const struct ops *ops) { return go_direct(d, d->x, ops); }
        void alloc(struct dev *d)
        {
            const struct ops *ops = get_ops(d);
            if (alloc_direct(d, ops))
                note();
            else if (ops->alloc)
                other();
            kept();
        }
        static inline void *err_ptr(long error) { return (void *)error; }
        static inline int is_err(const void *ptr)
        {
            return __builtin_expect(!!((unsigned long)(void *)(ptr) >= (unsigned long)-4095), 0);
        }
        static inline struct dev *get_gpio(struct dev *d) { warn(); return err_ptr(-38); }
        int probe(struct dev *d)
        {
            struct dev *gpio;
            gpio = get_gpio(d);
            if (is_err(gpio))
                return -1;
            late();
            return 0;
        }
        """,
    )
    alloc = facts["functions"]["alloc"]["events"]
    assert {call["callee"] for call in alloc["calls"]} == {"get_ops", "alloc_direct", "note", "kept"}
    assert not [item for item in alloc["field_accesses"] if item["field"] == "alloc"]
    assert "late" not in {call["callee"] for call in facts["functions"]["probe"]["events"]["calls"]}


def test_a_helper_whose_returns_are_several_constants_compares_as_each(tmp_path: Path) -> None:
    """softleaf_is_device_exclusive(entry): softleaf_type returns SOFTLEAF_NONE, SOFTLEAF_SWAP or
    SOFTLEAF_MARKER without CONFIG_DEVICE_PRIVATE, never SOFTLEAF_DEVICE_EXCLUSIVE."""
    facts = facts_of(
        tmp_path,
        """
        enum kind { K_NONE, K_SWAP, K_MIGRATE, K_DEVICE, K_MARKER };
        static inline int is_none(unsigned long v) { return v == 0; }
        static inline enum kind type_of(unsigned long v)
        {
            unsigned int n;
            if (is_none(v))
                return K_NONE;
            n = v >> 58;
            if (n < 30)
                return K_SWAP;
            switch (n) {
            case 31:
                return K_MARKER;
            }
            return K_NONE;
        }
        static inline int is_device(unsigned long v) { return type_of(v) == K_DEVICE; }
        static inline int is_swap(unsigned long v) { return type_of(v) == K_SWAP; }
        void copy(unsigned long v) { if (is_device(v)) other(); if (is_swap(v)) note(); kept(); }
        """,
    )
    assert {call["callee"] for call in facts["functions"]["copy"]["events"]["calls"]} == {
        "is_device", "is_swap", "note", "kept",
    }  # fmt: skip


def test_a_helper_s_value_on_its_parameters_guards_the_code_it_decides(tmp_path: Path) -> None:
    """warn_if_node_offline: `if (node_online(this_node)) return;`, node_state(node, N_ONLINE) is
    `node == 0` without NUMA, and folio_alloc passes numa_node_id(), cpu_to_node's `((void)(cpu),0)`.
    __vm_normal_page: `if (pgtable_level_has_pxx_special(level))`, a switch on the level that
    vm_normal_page passes as PGTABLE_LEVEL_PTE."""
    facts = facts_of(
        tmp_path,
        """
        extern int cpu;
        static inline int node_state(int node, int state) { return node == 0; }
        static inline int node_id(void) { return ((void)(cpu), 0); }
        static inline void offline(int this_node, gfp_t gfp)
        {
            if ((gfp & 6u) != 6u)
                return;
            if (node_state(this_node, 1))
                return;
            pr();
        }
        static inline void alloc_node(gfp_t gfp, int nid) { offline(nid, gfp); kept(); }
        void alloc(gfp_t gfp) { alloc_node(gfp, node_id()); }
        enum level { L_PTE, L_PMD, L_PUD };
        static inline int has_special(enum level level)
        {
            switch (level) {
            case L_PTE:
                return 1;
            case L_PMD:
                return 0;
            default:
                return 0;
            }
        }
        static inline void normal(struct dev *d, int special, enum level level)
        {
            if (has_special(level)) {
                if (special)
                    warn();
            } else if (d->x) {
                note();
            }
            kept();
        }
        """,
    )
    (call,) = [item for item in facts["functions"]["alloc"]["events"]["calls"] if item["callee"] == "alloc_node"]
    assert link._constant(call["arguments"][1]) == 0
    assert compiled(facts, "offline", [constant(0), constant(6)]) == {"node_state"}
    assert compiled(facts, "offline", [constant(1), constant(6)]) == {"node_state", "pr"}
    assert compiled(facts, "normal", [unknown(), unknown(), constant(0)]) == {"has_special", "warn", "kept"}
    assert compiled(facts, "normal", [unknown(), unknown(), constant(1)]) == {"has_special", "note", "kept"}


def test_two_arguments_that_read_one_value_compare_equal(tmp_path: Path) -> None:
    """switch_hrtimer_base: `this_cpu_base = this_cpu_ptr(&hrtimer_bases); new_cpu_base =
    get_target_base(this_cpu_base, pinned);`, which returns its base without CONFIG_HOTPLUG_CPU and
    NO_HZ_COMMON; `again:` past both, `new_cpu_base = this_cpu_base; goto again;`. Then
    hrtimer_suitable_target's `if (new_cpu_base == this_cpu_base) return true;` always returns."""
    facts = facts_of(
        tmp_path,
        """
        struct base { int online; long next; };
        struct base *this_ptr(void);
        static inline int is_online(struct base *b) { if (1) return 1; return b->online; }
        static inline struct base *target(struct base *b, int pinned) { if (!is_online(b)) return (void *)0; return b; }
        static int suitable(struct base *new_base, struct base *this_base)
        {
            if (new_base == this_base)
                return 1;
            note();
            return new_base->next > 0;
        }
        void switch_base(int pinned)
        {
            struct base *new_base, *this_base;
            this_base = this_ptr();
            new_base = target(this_base, pinned);
        again:
            if (!suitable(new_base, this_base)) {
                new_base = this_base;
                goto again;
            }
            kept();
        }
        void other_base(struct base *given)
        {
            struct base *one = this_ptr();
            struct base *two = this_ptr();
            suitable(one, two);
            suitable(given, given);
        }
        """,
    )
    calls = {name: facts["functions"][name]["events"]["calls"] for name in ("switch_base", "other_base")}
    (switched,) = [call for call in calls["switch_base"] if call["callee"] == "suitable"]
    assert compiled(facts, "suitable", switched["arguments"]) == set()
    two, same = [call for call in calls["other_base"] if call["callee"] == "suitable"]
    assert compiled(facts, "suitable", two["arguments"]) == {"note"}
    assert compiled(facts, "suitable", same["arguments"]) == set()
