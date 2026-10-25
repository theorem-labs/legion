"""Review probes for cgraph-link e40c6cef: what clang -O2 compiles, for the paths this delta touches."""

from __future__ import annotations

from pathlib import Path
from typing import Any

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
