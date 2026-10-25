"""C inputs whose code clang-21 -O2 keeps and the facts must keep too: the review probes of the flow and
evaluator rules (calls, globals and fields the compiled function still has)."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from test_guards import facts_of, fold


def compiled(facts: dict[str, Any], function: str) -> set[str]:
    events = facts["functions"][function]["events"]
    found = {call["callee"] for call in events["calls"]}
    found |= {item["unit"]["name"] for item in events["accesses"] if item["unit"]["kind"] == "global"}
    return found | {item["field"] for item in events["field_accesses"]}


CASES = """
    struct S2 { int field; };
    static int mode_once;
    void set_mode_once(void) { __asm__ volatile("sw zero, %0" : "=m"(mode_once)); }
    void use_mode_once(void) { if (mode_once) warn(); kept(); }
    static int mode_twice;
    static int mode_twice = 0;
    void set_mode_twice(void) { __asm__ volatile("sw zero, %0" : "=m"(mode_twice)); }
    void use_mode_twice(void) { if (mode_twice) warn(); kept(); }
    int asm_copy(struct S2 *p) { int x = p->field; int y = x; __asm__ volatile("" : "=r"(y)); return x - y; }
    void goto_live_after(void) { goto L; if (0) { L: warn(); } kept(); }
    void goto_block_after(void) { goto L; return; { L: warn(); } kept(); }
    static inline void asm_param(int x) { __asm__ volatile("li %0, 5" : "=r"(x)); if (x == 5) warn(); kept(); }
    void asm_param_zero(void) { asm_param(0); }
    void asm_move(int x) { int y = 0; __asm__("mv %0, %1" : "=r"(y) : "r"(x)); if (y == 7) warn(); kept(); }
    static __attribute__((noinline)) int sq(int x) { return x * 2; }
    static inline void chk(int k) { if (sq(k) == 6) warn(); kept(); }
    void ca(void) { chk(1); }
    void cb(void) { chk(3); }
    static __attribute__((noinline)) int sq3(int x) { return x * 2; }
    void tw(int c) { int k = c ? 1 : 2; if (sq3(k) == 6) warn(); kept(); }
    void tw2(void) { sq3(3); }
"""


def test_a_static_an_asm_writes_is_not_its_initializer(tmp_path: Path) -> None:
    assert "warn" in compiled(facts_of(tmp_path, CASES), "use_mode_once")


def test_a_redeclared_static_an_asm_writes_is_not_its_initializer(tmp_path: Path) -> None:
    """`static int mode; static int mode = 0;`: the asm names the second declaration, static_value asks
    for the first (canonical) one."""
    assert "warn" in compiled(facts_of(tmp_path, CASES), "use_mode_twice")


def test_a_copy_an_asm_overwrites_is_not_the_source(tmp_path: Path) -> None:
    """`int y = x; asm("" : "=r"(y)); return x - y;`: x - y is not 0, so p->field is loaded."""
    assert "field" in compiled(facts_of(tmp_path, CASES), "asm_copy")


def test_code_after_a_dead_statement_a_goto_enters_is_code(tmp_path: Path) -> None:
    """`goto L; if (0) { L: warn(); } kept();`: warn falls through to kept."""
    found = compiled(facts_of(tmp_path, CASES), "goto_live_after")
    assert {"warn", "kept"} <= found


def test_code_after_a_block_a_goto_enters_is_code(tmp_path: Path) -> None:
    found = compiled(facts_of(tmp_path, CASES), "goto_block_after")
    assert {"warn", "kept"} <= found


def test_a_parameter_an_asm_writes_is_not_the_argument(tmp_path: Path) -> None:
    facts = facts_of(tmp_path, CASES)
    assert "warn" in {call["callee"] for call in fold(facts, "asm_param_zero", "asm_param").calls}


def test_an_asm_output_is_not_the_local_s_initializer(tmp_path: Path) -> None:
    assert "warn" in compiled(facts_of(tmp_path, CASES), "asm_move")


def test_a_noinline_callee_s_value_is_not_its_template_at_a_fold(tmp_path: Path) -> None:
    facts = facts_of(tmp_path, CASES)
    assert "warn" in {call["callee"] for call in fold(facts, "ca", "chk").calls}


def test_a_noinline_callee_s_value_is_not_its_template(tmp_path: Path) -> None:
    assert "warn" in compiled(facts_of(tmp_path, CASES), "tw")


ROUND_SIX = """
    #define NEVER_INLINE __attribute__((noinline))
    void exit_else(void) { goto L; if (1) return; else { L: warn(); } kept(); }
    static int half(int x);
    static __attribute__((noinline)) int half(int x);
    static int half(int x) { return x / 2; }
    void declared_noinline(void) { if (half(4) == 7) warn(); kept(); }
    NEVER_INLINE static int third(int x) { return x / 3; }
    void macro_noinline(void) { if (third(3) == 7) warn(); kept(); }
"""


def test_code_after_an_exit_statement_a_goto_enters_is_code(tmp_path: Path) -> None:
    """`goto L; if (1) return; else { L: warn(); } kept();`: entry at L falls out to kept."""
    assert {"warn", "kept"} <= compiled(facts_of(tmp_path, ROUND_SIX), "exit_else")


def test_a_noinline_an_earlier_declaration_or_a_macro_writes_is_called(tmp_path: Path) -> None:
    """noinline on an earlier declaration, or written by a NEVER_INLINE macro: clang calls the function
    and keeps `call warn` after the comparison."""
    facts = facts_of(tmp_path, ROUND_SIX)
    assert "warn" in compiled(facts, "declared_noinline")
    assert "warn" in compiled(facts, "macro_noinline")


ROUND_SIX_CLAUDE = """
    #define noinline __attribute__((__noinline__))
    #define noinline_for_stack noinline
    #define notrace __attribute__((__no_instrument_function__))
    #define noinstr noinline notrace __attribute__((__section__(".noinstr.text")))
    void step(int i);
    static noinline_for_stack int dbl_stack(int x) { return x * 2; }
    static inline void chk_stack(int k) { if (dbl_stack(k) == 6) warn(); kept(); }
    void sa(void) { chk_stack(1); }
    static noinstr int dbl_instr(int x) { return x * 2; }
    void si(void) { if (dbl_instr(1) == 6) warn(); kept(); }
    static int dbl_late(int x) __attribute__((__noinline__));
    static int dbl_late(int x) { return x * 2; }
    void sl(void) { if (dbl_late(1) == 6) warn(); kept(); }
    static int big(int x)
    {
        for (int i = 0; i < x + 64; i++) {
            step(i); step(i + 1); step(i + 2); step(i + 3); step(i + 4); step(i + 5); step(i + 6);
            step(i + 7); step(i + 8); step(i + 9); step(i + 10); step(i + 11); step(i + 12); step(i + 13);
            step(i + 14);
        }
        return x * 2;
    }
    void ba(void) { if (big(1) == 6) warn(); kept(); }
    void bb(void) { if (big(3) == 6) warn(); kept(); }
    static int once(int x) { return x * 2; }
    void oa(void) { if (once(1) == 6) warn(); kept(); }
    void two_way_a(void) { goto L; if (1) { return; } else { L: warn(); } kept(); }
    void two_way_b(void) { goto L; if (0) { L: warn(); } else return; kept(); }
"""


@pytest.mark.parametrize("function", ["si", "sl", "ba", "bb", "two_way_a", "two_way_b"])
def test_round_six_keeps_what_clang_keeps(tmp_path: Path, function: str) -> None:
    """A noinline the kernel's wrapper macros or a later attribute write, a static with two call sites
    clang does not inline (big), and both shapes of a goto into one branch of a constant if: clang-21
    -O2 keeps `call warn` and `kept` in each."""
    facts = facts_of(tmp_path, ROUND_SIX_CLAUDE)
    assert {"warn", "kept"} <= compiled(facts, function)


def test_a_noinline_for_stack_callee_is_called_at_a_fold(tmp_path: Path) -> None:
    facts = facts_of(tmp_path, ROUND_SIX_CLAUDE)
    assert "warn" in {call["callee"] for call in fold(facts, "sa", "chk_stack").calls}


def test_a_static_with_one_call_site_still_folds(tmp_path: Path) -> None:
    """`once` has one call site, which clang inlines: `once(1) == 6` is false there."""
    assert "warn" not in compiled(facts_of(tmp_path, ROUND_SIX_CLAUDE), "oa")


def test_a_scoped_noinline_attribute_is_read(tmp_path: Path) -> None:
    """`[[gnu::noinline]] static int sq(int x)`: clang calls sq and keeps `tail warn`, which a folded
    `sq(1) == 6` would rule out."""
    facts = facts_of(
        tmp_path, "[[gnu::noinline]] static int sq(int x) { return x * 2; } void sc(void) { if (sq(1) == 6) warn(); }"
    )
    assert "warn" in compiled(facts, "sc")


OUT_OF_LINE = """
    void consume(int);
    static int vsmall(int x, ...)
    {
        __builtin_va_list ap;
        __builtin_va_start(ap, x);
        consume(__builtin_va_arg(ap, int));
        __builtin_va_end(ap);
        return x * 2;
    }
    void v_root(void) { if (vsmall(1, 11) == 6) warn(); }
    void v_other(void) { if (vsmall(3, 22) == 6) warn(); }
    static __attribute__((optnone)) int osmall(int x) { return x * 2; }
    void o_root(void) { if (osmall(1) == 6) warn(); }
    void o_other(void) { if (osmall(3) == 6) warn(); }
    static __attribute__((cold)) int csmall(int x) { consume(x); consume(x + 1); consume(x + 2); consume(x + 3); return x * 2; }
    void c_root(void) { if (csmall(1) == 6) warn(); }
    void c_other(void) { if (csmall(3) == 6) warn(); }
"""


@pytest.mark.parametrize("function", ["v_root", "o_root", "c_root"])
def test_a_variadic_optnone_or_cold_callee_is_called_out_of_line(tmp_path: Path, function: str) -> None:
    """A variadic helper, an optnone one and a cold one, each with two callers: clang-21 -O2 calls each
    and keeps `tail warn`, which a folded `x * 2 == 6` for 1 would rule out."""
    assert "warn" in compiled(facts_of(tmp_path, OUT_OF_LINE), function)


WRAPPED = """
    void step(int i);
    static inline __attribute__((always_inline)) void heavy(int x)
    {
        step(x); step(x + 1); step(x + 2); step(x + 3); step(x + 4); step(x + 5); step(x + 6); step(x + 7);
        step(x + 8); step(x + 9); step(x + 10); step(x + 11); step(x + 12); step(x + 13); step(x + 14);
        step(x + 15); step(x + 16); step(x + 17); step(x + 18); step(x + 19);
    }
    static int wrap(int x) { heavy(x); return x * 2; }
    void wa(void) { if (wrap(1) == 6) warn(); kept(); }
    void wb(void) { if (wrap(3) == 6) warn(); kept(); }
    static inline int stub_variadic(const char *fmt, ...) { return 0; }
    void sv(void) { if (stub_variadic("x", 1)) warn(); kept(); }
"""


def test_a_small_function_that_calls_a_large_inline_one_is_called(tmp_path: Path) -> None:
    """wrap() calls an __always_inline heavy() of 20 calls: clang inlines heavy into wrap first, then
    finds wrap too large and calls it (clang-21 -O2: call wrap; call warn)."""
    assert "warn" in compiled(facts_of(tmp_path, WRAPPED), "wa")


def test_a_variadic_stub_that_never_starts_its_list_still_folds(tmp_path: Path) -> None:
    """A variadic stub with no va_start is inlined: `stub_variadic(...)` is 0, so warn is not code."""
    assert "warn" not in compiled(facts_of(tmp_path, WRAPPED), "sv")


MODELLED = """
    void s(int i);
    static void inner(int x) { s(x); s(x + 1); s(x + 2); s(x + 3); }
    static int stat_wrap(int x) { inner(x); inner(x + 1); inner(x + 2); inner(x + 3); return x * 2; }
    void pa(void) { if (stat_wrap(1) == 6) warn(); kept(); }
    void pb(void) { if (stat_wrap(3) == 6) warn(); kept(); }
    static int leaf(int x) { s(x); s(x + 1); s(x + 2); s(x + 3); return x; }
    static int outer(int x) { leaf(x); leaf(x + 1); leaf(x + 2); return x * 2; }
    void la(void) { if (outer(1) == 6) warn(); kept(); }
    void lb(void) { if (outer(3) == 6) warn(); kept(); }
"""


@pytest.mark.parametrize("function", ["pa", "la"])
def test_a_wrapper_around_small_static_helpers_is_called(tmp_path: Path, function: str) -> None:
    """clang inlines bottom-up: a small static helper goes into its caller first, and the caller, now
    large, is called (clang-21 -O2: call stat_wrap; call warn). Its value is the image's to settle, so
    warn() stays, as `conditional`."""
    assert "warn" in compiled(facts_of(tmp_path, MODELLED), function)
