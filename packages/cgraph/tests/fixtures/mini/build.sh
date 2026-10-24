#!/usr/bin/env bash
# Rebuilds image/mini.elf and image/System.map from src/ and include/ with clang and ld.lld.
# The tests read the committed outputs; run this only after changing the sources, then commit both.
#   CLANG=clang-21 LLD=ld.lld-21 NM=llvm-nm-21 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
CLANG="${CLANG:-clang}"
LLD="${LLD:-ld.lld}"
NM="${NM:-llvm-nm}"
FLAGS=(--target=riscv64-unknown-elf -march=rv64imac -mabi=lp64 -mcmodel=medany -O2 -ffreestanding
	-nostdinc -fno-builtin -fms-extensions -Iinclude -std=gnu2x)
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for source in src/store.c src/bags.c src/flow.c src/facts.c src/dead.c src/anchor.S src/fold.c; do
	"$CLANG" "${FLAGS[@]}" -c "$source" -o "$work/$(basename "$source").o"
done
# The stubs give the external helpers a body so the image links; the tests never read them.
cat >"$work/stubs.c" <<'EOF'
void spin_lock(void *s) { (void)s; }
void spin_unlock(void *s) { (void)s; }
void down_write(void *s) { (void)s; }
void up_write(void *s) { (void)s; }
static char heap[4096];
void *obj_alloc(unsigned long size) { (void)size; return heap; }
void obj_free(void *p) { (void)p; }
void box_release(void *b) { (void)b; }
void fct_die(void) { for (;;) ; }
void fct_takes_inner(void *in) { (void)in; }
void fct_takes_node(void *n) { (void)n; }
void *fct_other(int k) { (void)k; return heap; }
void fct_release(void *in) { (void)in; }
void *fct_rq_lock(void *p, void *rf) { (void)p; (void)rf; return heap; }
void fct_rq_unlock(void *rq, void *rf) { (void)rq; (void)rf; }
int fct_is_link(const void *n) { return n != 0; }
void *fct_find(void) { return heap; }
void *fct_cur(void) { return heap; }
void fct_show(unsigned long m) { (void)m; }
void _start(void) { for (;;) ; }
EOF
"$CLANG" "${FLAGS[@]}" -c "$work/stubs.c" -o "$work/stubs.o"
# src/fold.c comes after the stubs: code added there moves no address before it.
"$LLD" -o image/mini.elf --build-id=none -Tpercpu.ld "$work/store.c.o" "$work/bags.c.o" "$work/flow.c.o" \
	"$work/facts.c.o" "$work/dead.c.o" "$work/anchor.S.o" "$work/stubs.o" "$work/fold.c.o"
# Like the kernel's System.map, the map leaves out assembler-local labels (.L...).
"$NM" -n image/mini.elf | awk 'NF == 3 && $3 !~ /^\.L/' >image/System.map
