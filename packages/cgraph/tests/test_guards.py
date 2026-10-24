"""The C facts' guards: the conditions on a function's parameters under which each event is compiled,
and the values call arguments pass, as link reads them at a fold (link._bind)."""

from __future__ import annotations

import itertools
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


def test_a_call_through_a_parameter_gives_its_arguments_and_a_function_argument_its_name(tmp_path: Path) -> None:
    """bdi_put: kref_put(&bdi->refcnt, release_bdi); kref_put calls release(kref) when the count drops."""
    facts = facts_of(
        tmp_path,
        """
        struct kref { int refcount; };
        struct bdi { long id[8]; struct kref refcnt; long state; };
        static inline int put(struct kref *kref, void (*release)(struct kref *kref))
        {
            if (--kref->refcount == 0) {
                release(kref);
                return 1;
            }
            return 0;
        }
        static void release_bdi(struct kref *ref) { note(); }
        void bdi_put(struct bdi *bdi) { put(&bdi->refcnt, release_bdi); }
        """,
    )
    (through,) = facts["functions"]["put"]["events"]["parameter_calls"]
    (kref,) = through["arguments"]
    assert (through["param"], kref["param"], kref["displacement"]) == (1, 0, 0)
    (call,) = facts["functions"]["bdi_put"]["events"]["calls"]
    assert call["arguments"][1]["function"] == "release_bdi"
    assert (call["arguments"][0]["param"], call["arguments"][0]["displacement"]) == (0, 64)


def test_an_operand_a_parameter_may_zero_is_read_only_where_it_does_not(tmp_path: Path) -> None:
    """__fget_files_rcu: `if (unlikely(file->f_mode & mask))`, and fdget_raw passes mask 0: the
    compiler never reads f_mode there. A call with an effect is made whatever the mask is."""
    facts = facts_of(
        tmp_path,
        """
        struct file { unsigned int f_mode; long f_count; };
        unsigned int count(struct file *file);
        static inline struct file *get(struct file *file, unsigned int mask)
        {
            if (__builtin_expect(!!(file->f_mode & mask), 0)) {
                pr();
                return (void *)0;
            }
            if (count(file) & mask)
                return (void *)0;
            return file;
        }
        """,
    )
    record = facts["functions"]["get"]
    (mode,) = [item for item in record["events"]["field_accesses"] if item["field"] == "f_mode"]
    guards = [record["guards"][index] for index in mode.get("guards", [])]
    assert link._decided(guards, [unknown(), constant(0)]) is None
    assert link._decided(guards, [unknown(), constant(2)]) == []
    assert compiled(facts, "get", [unknown(), constant(0)]) == {"count"}


def test_an_argument_that_names_a_global_gives_the_lines_it_spans(tmp_path: Path) -> None:
    """vmap_init_free_space: insert_vmap_area_augment(free, 0,\\n &free_vmap_area_root,\\n
    &free_vmap_area_list): the accesses of each argument are on its own lines."""
    facts = facts_of(
        tmp_path,
        """
        struct root { long node; };
        static struct root tree;
        static long counter;
        void insert(long v, struct root *r, long *c);
        void init(long v)
        {
            insert(v,
                   &tree,
                   &counter);
        }
        """,
    )
    (call,) = facts["functions"]["init"]["events"]["calls"]
    first = facts["functions"]["init"]["line"]
    assert [argument.get("lines") for argument in call["arguments"]] == [
        None,
        [first + 3, first + 3],
        [first + 4, first + 4],
    ]


def fold(facts: dict[str, Any], *chain: str) -> link.Effective:
    """The events of the last function of ``chain`` inside the first: each call along the chain folded
    with the arguments it passes, as link folds inline helpers (link._bind)."""
    record = facts["functions"][chain[-1]]
    table = record.get("guards", [])
    events = {kind: link._resolved(record["events"][kind], table) for kind in ("calls", *link._EVENT_LISTS)}
    folded = link.Effective(**events)
    for caller, callee in reversed(list(itertools.pairwise(chain))):
        (call,) = [item for item in facts["functions"][caller]["events"]["calls"] if item["callee"] == callee]
        folded = link._bind(folded, call["arguments"])
    return folded


BY_VALUE = """
    struct params { unsigned short len; unsigned short offset; long (*fn)(long, unsigned int); };
    struct table { long pad[2]; struct params p; };
    static struct table tab;
    static const struct params tab_params = { .len = 8, .offset = 4 };
    long other_hash(long x);
    static inline long hash(struct table *t, long x, const struct params params)
    {
        return __builtin_expect(!!(params.fn), 1) ? params.fn(x, params.len ?: t->p.len) : other_hash(x + params.offset);
    }
    static inline long mid(struct table *t, long x, const struct params params) { return hash(t, x, params); }
    long add(long x) { return mid(&tab, x, tab_params); }
    long add_given(struct table *t, long x, struct params given) { return mid(t, x, given); }
"""


def test_a_member_of_a_struct_passed_by_value_guards_and_a_const_global_struct_decides_it(tmp_path: Path) -> None:
    """rht_head_hashfn: `likely(params.obj_hashfn) ? …params.obj_hashfn(ptr, params.key_len ?: ht->p.key_len,
    …) : rht_key_hashfn(…, ptr + params.key_offset, params)`; pidfs_add_pid passes the static const
    pidfs_ino_ht_params, whose obj_hashfn is NULL (llvm-objdump of add: addi a0, a0, 0x4; tail other_hash)."""
    facts = facts_of(tmp_path, BY_VALUE)
    added = fold(facts, "add", "mid", "hash")
    assert "len" not in {item["field"] for item in added.field_accesses}
    assert ({call["callee"] for call in added.calls}, added.indirect) == ({"other_hash"}, [])
    given = fold(facts, "add_given", "mid", "hash")
    assert ("len", 16) in {(item["field"], item["displacement"]) for item in given.field_accesses}
    assert [item["field"] for item in given.indirect] == ["fn"]


ADDRESS = """
    struct vp { long a; long rm; long rm2; };
    struct mg { int del; int nxt; long v; long w; };
    static inline void prep(struct vp *p, struct mg *m)
    {
        long *r = &p->rm;
        if (m && m->del) {
            *r = m->v;
            r = &p->rm2;
        }
        if (m && m->nxt)
            *r = m->w;
    }
    static inline void either(struct vp *p, int k) { long *r = &p->rm; if (k == 1) *r = 1; if (k == 2) *r = 2; }
    static inline void always(struct vp *p, struct mg *m) { long *r = &p->rm; if (m && m->del) *r = m->v; *r += 1; }
"""


def test_an_address_a_local_holds_is_formed_only_where_a_read_of_the_local_is_compiled(tmp_path: Path) -> None:
    """init_multi_vma_prep: `remove = &vp->remove;`, read only under `vmg && vmg->__remove_middle` or
    `vmg && vmg->__remove_next`; init_vma_prep passes vmg NULL, so vma_shrink forms no &vp->remove."""
    facts = facts_of(tmp_path, ADDRESS)

    def rm_guards(function: str) -> list[dict[str, Any]]:
        record = facts["functions"][function]
        (rm,) = [item for item in record["events"]["field_accesses"] if item["field"] == "rm"]
        return [record["guards"][index] for index in rm.get("guards", [])]

    assert link._decided(rm_guards("prep"), [unknown(), constant(0)]) is None
    assert link._decided(rm_guards("prep"), [unknown(), unknown()]) is not None
    assert link._decided(rm_guards("either"), [unknown(), constant(3)]) is None
    assert link._decided(rm_guards("either"), [unknown(), constant(2)]) == []
    assert link._decided(rm_guards("always"), [unknown(), constant(0)]) == []


GLOBAL_ADDRESS = """
    struct mm { long x; };
    extern struct mm init_mm;
    extern struct mm weak_mm __attribute__((weak));
    struct pt { int lock; };
    static inline int lock_init(struct pt *p) { p->lock = 0; return 1; }
    static inline int ctor(struct mm *mm, struct pt *p)
    {
        if (mm != &init_mm && !lock_init(p))
            return 0;
        note();
        return 1;
    }
    static inline int weak_ctor(struct mm *mm, struct pt *p)
    {
        if (mm != &weak_mm && !lock_init(p))
            return 0;
        note();
        return 1;
    }
    void alloc_late(struct pt *p) { if (!ctor((void *)0, p)) warn(); }
    void alloc_init(struct pt *p) { if (!ctor(&init_mm, p)) warn(); }
    void alloc_given(struct mm *mm, struct pt *p) { if (!ctor(mm, p)) warn(); }
    void alloc_weak(struct pt *p) { if (!weak_ctor((void *)0, p)) warn(); }
"""


def test_a_global_s_address_a_constant_argument_compares_with_is_not_formed(tmp_path: Path) -> None:
    """alloc_pte_late: `pagetable_pte_ctor(NULL, ptdesc)`, whose `mm != &init_mm && !ptlock_init(ptdesc)`
    is true for NULL (llvm-objdump of alloc_late: sw zero, 0x0(a0); tail note). A weak object's address
    may be NULL, so alloc_weak forms &weak_mm and tests it (auipc, ld, beqz)."""
    facts = facts_of(tmp_path, GLOBAL_ADDRESS)

    def globals_read(*chain: str) -> set[str]:
        return {item["unit"]["name"] for item in fold(facts, *chain).accesses if item["unit"]["kind"] == "global"}

    assert globals_read("alloc_late", "ctor") == set()
    assert "init_mm" in {(item.get("unit") or {}).get("name") for item in fold(facts, "alloc_late", "ctor").unevaluated}
    assert globals_read("alloc_given", "ctor") == {"init_mm"}
    assert globals_read("alloc_weak", "weak_ctor") == {"weak_mm"}
    # &init_mm != &init_mm is false: the right operand is not computed.
    assert "lock_init" not in {call["callee"] for call in fold(facts, "alloc_init", "ctor").calls}
    assert "lock_init" in {call["callee"] for call in fold(facts, "alloc_late", "ctor").calls}


FUNCTION_MEMBER = """
    struct hp { unsigned short len; long (*hash)(const void *, unsigned int); int (*cmp)(const void *, const void *); };
    struct ht { long pad[2]; struct hp p; };
    long other_hash(const void *k, unsigned int len);
    int compare(const void *a, const void *b);
    static long name_hash(const void *k, unsigned int len) { return *(const long *)k; }
    static int name_cmp(const void *a, const void *b) { return a == b; }
    static const struct hp named_params = { .hash = name_hash, .cmp = name_cmp };
    static inline long lookup(struct ht *t, const void *k, const struct hp params)
    {
        long h = __builtin_expect(!!(params.hash), 1) ? params.hash(k, params.len ?: t->p.len) : other_hash(k, t->p.len);
        return params.cmp ? h + params.cmp(k, t) : h + compare(k, t);
    }
    long find(struct ht *t, const void *k) { return lookup(t, k, named_params); }
    long find_given(struct ht *t, const void *k, struct hp given) { return lookup(t, k, given); }
"""


def test_a_const_struct_s_function_member_decides_its_tests_and_leaves_an_argument_it_ignores_to_the_image(
    tmp_path: Path,
) -> None:
    """simple_xattr_add: simple_xattr_params' obj_hashfn and obj_cmpfn are functions, so rht_head_hashfn's
    `likely(params.obj_hashfn)` and `params.obj_cmpfn ?` hold, and simple_xattr_obj_hashfn never reads
    the key_len it is passed (llvm-objdump of find: ld a2, 0x0(a1); xor; seqz; add; ret)."""
    facts = facts_of(tmp_path, FUNCTION_MEMBER)

    def lengths(found: link.Effective) -> list[Any]:
        return sorted(str(item.get("conditional")) for item in found.field_accesses if item.get("base") == "pointer")

    found = fold(facts, "find", "lookup")
    assert {call["callee"] for call in found.calls} == set()
    assert lengths(found) == ["True"]
    given = fold(facts, "find_given", "lookup")
    assert {call["callee"] for call in given.calls} == {"other_hash", "compare"}
    assert lengths(given) == ["None", "None"]


SEQPROP = """
    typedef struct { unsigned sequence; } seqcount_t;
    typedef struct { seqcount_t seqcount; int *lock; } seqcount_raw_spinlock_t;
    typedef struct { seqcount_t seqcount; int *lock; } seqcount_mutex_t;
    static inline _Bool __seqprop_preemptible(const seqcount_t *s) { return 0; }
    #define SEQ_LOCKNAME(lockname, preemptible) \\
    static inline _Bool __seqprop_##lockname##_preemptible(const seqcount_##lockname##_t *s) { return preemptible; }
    SEQ_LOCKNAME(raw_spinlock, 0)
    SEQ_LOCKNAME(mutex, 1)
    #define __seqprop_case(s, lockname, prop) seqcount_##lockname##_t: __seqprop_##lockname##_##prop
    #define __seqprop(s, prop) _Generic(*(s), seqcount_t: __seqprop_##prop, \\
        __seqprop_case((s), raw_spinlock, prop), __seqprop_case((s), mutex, prop))
    #define seqprop_preemptible(s) __seqprop(s, preemptible)(s)
    #define write_begin(s) do { if (seqprop_preemptible(s)) note(); } while (0)
    void seq_raw(seqcount_raw_spinlock_t *s) { write_begin(s); kept(); }
    void seq_plain(seqcount_t *s) { write_begin(s); kept(); }
    void seq_mutex(seqcount_mutex_t *s) { write_begin(s); kept(); }
"""


def test_a_call_a_generic_selection_names_returns_what_the_selected_function_returns(tmp_path: Path) -> None:
    """write_seqcount_begin(&tkd->seq) on a seqcount_raw_spinlock_t: seqprop_preemptible(s) is
    _Generic(*(s), …)(s), which selects __seqprop_raw_spinlock_preemptible, `return false` (llvm-objdump
    of seq_raw and seq_plain: tail kept)."""
    facts = facts_of(tmp_path, SEQPROP)

    def callees(function: str) -> set[str]:
        return {call["callee"] for call in facts["functions"][function]["events"]["calls"]}

    assert "note" not in callees("seq_raw") | callees("seq_plain")
    assert "note" in callees("seq_mutex")


COMPOUND_LITERAL = """
    typedef struct { unsigned long pte; } pte_t;
    #define __pte(x) ((pte_t) { (x) })
    static inline int pte_present(pte_t pte) { return pte.pte & 1; }
    static inline void set_pte_at(pte_t pteval) { if (pte_present(pteval)) warn(); kept(); }
    static inline void pte_clear(void) { set_pte_at(__pte(0)); }
    static inline void pte_mark(void) { set_pte_at(__pte(3)); }
    static inline void pte_set(pte_t pte) { set_pte_at(pte); }
"""


def test_a_compound_literal_argument_is_a_constant_struct(tmp_path: Path) -> None:
    """pte_clear: `__set_pte_at(mm, ptep, __pte(0))`, `__pte(x)` being `((pte_t) { (x) })`: pte_present of
    it is 0, so flush_icache_pte is not called (llvm-objdump of __set_fixmap: sd zero, no call)."""
    facts = facts_of(tmp_path, COMPOUND_LITERAL)
    assert {call["callee"] for call in fold(facts, "pte_clear", "set_pte_at").calls} == {"pte_present", "kept"}
    assert "warn" in {call["callee"] for call in fold(facts, "pte_mark", "set_pte_at").calls}
    assert "warn" in {call["callee"] for call in fold(facts, "pte_set", "set_pte_at").calls}


FLOW = """
    static inline int stub_ns(void *p) { return -22; }
    int set_counts(void *p);
    void bump(int *v);
    int copy(void *p, unsigned long flags)
    {
        int ret;
        if (flags & 1) {
            ret = stub_ns(p);
            if (ret < 0)
                goto error;
            ret = set_counts(p);
            if (ret < 0)
                goto error;
        }
        late();
        return 0;
    error:
        err();
        return ret;
    }
    int copy_bumped(void *p)
    {
        int ret = stub_ns(p);
        bump(&ret);
        if (ret < 0)
            return ret;
        note();
        return 0;
    }
    static inline int *trans_lock(void *p) { return 0; }
    int *map_lock(void *p, int **ptl);
    void range(void *p) { int *ptl; ptl = trans_lock(p); if (ptl) { warn(); return; } map_lock(p, &ptl); kept(); }
    static inline int highmem(void *p) { return 0; }
    void remap_fn(void *p) { int remap = 0; if (highmem(p)) remap = 1; if (remap) warn(); kept(); }
    void remap_live(void *p, int k) { int remap = 0; if (k) remap = 1; if (remap) warn(); kept(); }
"""


def test_a_local_holds_what_the_stores_that_reach_a_use_on_live_paths_give(tmp_path: Path) -> None:
    """copy_creds: `ret = create_user_ns(new); if (ret < 0) goto error_put;` with the stub returning
    -EINVAL, so set_cred_ucounts is not compiled; mlock_pte_range: `ptl = pmd_trans_huge_lock(pmd,
    vma); if (ptl)` with the stub returning NULL, `&ptl` taken after; dma_direct_alloc: `remap = true`
    only under PageHighMem, which is 0 (llvm-objdump: no call of set_counts, warn)."""
    facts = facts_of(tmp_path, FLOW)

    def callees(function: str) -> set[str]:
        return {call["callee"] for call in facts["functions"][function]["events"]["calls"]}

    assert callees("copy") == {"stub_ns", "late", "err"}
    assert "note" in callees("copy_bumped")
    assert "warn" not in callees("range")
    assert "warn" not in callees("remap_fn")
    assert "warn" in callees("remap_live")


WARN_ONCE = """
    void __warn_printk(const char *fmt, ...);
    #define unlikely(x) __builtin_expect(!!(x), 0)
    #define __ONCE_LITE_IF(condition) ({ static _Bool __already_done; _Bool __ret_cond = !!(condition); \\
        _Bool __ret_once = 0; if (unlikely(__ret_cond && !__already_done)) { __already_done = 1; \\
        __ret_once = 1; } unlikely(__ret_once); })
    #define DO_ONCE_LITE_IF(condition, func, ...) ({ _Bool __ret_do_once = !!(condition); \\
        if (__ONCE_LITE_IF(__ret_do_once)) func(__VA_ARGS__); unlikely(__ret_do_once); })
    #define WARN(condition, format...) ({ int __ret_warn_on = !!(condition); \\
        if (unlikely(__ret_warn_on)) __warn_printk(format); unlikely(__ret_warn_on); })
    #define WARN_ONCE(condition, format...) DO_ONCE_LITE_IF(condition, WARN, 1, format)
    void lock_idx(unsigned long mask) { WARN_ONCE(0 && (mask & (mask - 1)), "mixed"); kept(); }
    void lock_live(unsigned long mask) { WARN_ONCE(mask & (mask - 1), "mixed"); kept(); }
    void size_table(long *table, int min)
    {
        int i;
        for (i = 8; i < 8; i += 8)
            table[i] = note_index(i);
        for (i = 8; i < min; i += 8)
            table[i] = late_index(i);
        kept();
    }
    int note_index(int i); int late_index(int i);
"""


def test_a_warn_once_whose_condition_is_constant_false_and_a_loop_never_entered_are_not_code(tmp_path: Path) -> None:
    """srcu_readers_lock_idx: `WARN_ONCE(IS_ENABLED(CONFIG_PROVE_RCU) && (mask & (mask - 1)), …)`:
    __ONCE_LITE_IF's `__ret_once` is set only under its false test, so its value, 0, decides the if
    (llvm-objdump: no __warn_printk). setup_kmalloc_cache_index_table: `for (i = 8; i < KMALLOC_MIN_SIZE;
    i += 8)` with KMALLOC_MIN_SIZE 8 (llvm-objdump: ret)."""
    facts = facts_of(tmp_path, "int note_index(int i); int late_index(int i);" + WARN_ONCE)

    def callees(function: str) -> set[str]:
        return {call["callee"] for call in facts["functions"][function]["events"]["calls"]}

    assert callees("lock_idx") == {"kept"}
    assert "__warn_printk" in callees("lock_live")
    assert callees("size_table") == {"late_index", "kept"}


MEMBER_CALL = """
    struct hp { unsigned short len; long (*hash)(const void *, unsigned int); };
    long inner(const void *k);
    static long name_hash(const void *k, unsigned int len) { return inner(k); }
    static const struct hp named = { .hash = name_hash };
    static inline long lookup(const void *k, const struct hp params) { return params.hash ? params.hash(k, params.len) : 0; }
    static inline long mid(const void *k, const struct hp params) { return lookup(k, params); }
    long find(const void *k) { return mid(k, named); }
"""


def test_a_function_a_const_struct_member_names_is_folded_where_a_helper_calls_through_it(tmp_path: Path) -> None:
    """simple_xattr_add: rht_head_hashfn calls `params.obj_hashfn(ptr, …)`, simple_xattr_params names
    simple_xattr_obj_hashfn, which the compiler inlined: the image of simple_xattr_add calls its jhash."""
    from test_link_rules import StubImage, build, c_calls, call_pairs, image_calls

    from cgraph.image import FunctionFacts

    facts = facts_of(tmp_path, MEMBER_CALL)
    image = StubImage(
        {"find": [(0x100, False, "")], "inner": [(0x200, False, "")]},
        {0x100: FunctionFacts(calls=image_calls("inner"))},
    )
    graph = build({"src/g.c": facts}, image)
    assert "function:inner" in c_calls(graph, "function:find")
    assert call_pairs(graph)["only_image"] == []
