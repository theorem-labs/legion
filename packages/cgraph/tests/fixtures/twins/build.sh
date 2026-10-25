#!/usr/bin/env bash
# Rebuilds image/twins.elf and image/System.map from src/ with clang and ld.lld.
# The tests read the committed outputs; run this only after changing the sources, then commit both.
#   CLANG=clang-21 LLD=ld.lld-21 NM=llvm-nm-21 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
CLANG="${CLANG:-clang}"
LLD="${LLD:-ld.lld}"
NM="${NM:-llvm-nm}"
FLAGS=(--target=riscv64-unknown-elf -march=rv64imac -mabi=lp64 -mcmodel=medany -O2 -ffreestanding
	-nostdinc -fno-builtin -std=gnu2x)
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for source in src/a.c src/b.c; do
	"$CLANG" "${FLAGS[@]}" -c "$source" -o "$work/$(basename "$source").o"
done
"$LLD" -o image/twins.elf --build-id=none -e left_entry "$work/a.c.o" "$work/b.c.o"
# Like the kernel's System.map, the map leaves out assembler-local labels (.L...).
"$NM" -n image/twins.elf | awk 'NF == 3 && $3 !~ /^\.L/' >image/System.map
