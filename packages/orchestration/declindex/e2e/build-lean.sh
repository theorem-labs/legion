#!/usr/bin/env bash
set -euo pipefail

repository="${1:?usage: build-lean.sh <repository> <commit> <dir>}"
commit="${2:?usage: build-lean.sh <repository> <commit> <dir>}"
dir="${3:?usage: build-lean.sh <repository> <commit> <dir>}"

elevate=()
if [ "$(id -u)" -ne 0 ]; then
  elevate=(sudo)
fi

"${elevate[@]}" apt-get update
"${elevate[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends \
  build-essential \
  cmake \
  libgmp-dev \
  libuv1-dev \
  libssl-dev \
  pkg-config \
  git \
  zstd

git clone --filter=blob:none "$repository" "$dir"
git -C "$dir" checkout "$commit"
(
  cd "$dir"
  cmake --preset release
)
make -C "$dir/build/release" stage2 -j"$(nproc)"
"$dir/build/release/stage2/bin/lean" --version | grep --fixed-strings "$commit"
