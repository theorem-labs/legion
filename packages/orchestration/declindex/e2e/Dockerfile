# Build with the repository root as context. The Lean toolchain is copied prebuilt from the worker image; this image
# never builds Lean from source (build-lean.sh is for CI runners). The base is Debian trixie because the prebuilt
# toolchain links the system libgmp (trixie ships GMP 6.3.0, which Lean requires) and libstdc++ of trixie.
#
# LEAN_IMAGE (the worker image that holds /opt/lean/<LEAN_FORK_COMMIT>, by digest) and LEAN_TOOLCHAIN (the name in
# the served tree's lean-toolchain file) are operator inputs with no default: the build stops when either is unset.
ARG GO_IMAGE=mirror.gcr.io/library/golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734
ARG LEAN_IMAGE
FROM ${LEAN_IMAGE:?is_required:pass_--build-arg_LEAN_IMAGE=<worker-image>@sha256:<digest>} AS lean

FROM ${GO_IMAGE}

ARG LEAN_FORK_COMMIT=d71bb0c6d04adbb8e94adfdf8c0e3291bfeea831
ARG LEAN_TOOLCHAIN
ARG ELAN_VERSION=4.2.4
ARG ELAN_SHA256=42b94d4244e8353142c456ec0e4ca6528fd898a6c604d4059f494e706e431f63
ENV LEAN_TOOLCHAIN=${LEAN_TOOLCHAIN:?is_required:pass_--build-arg_LEAN_TOOLCHAIN=<the_served_tree_lean-toolchain>}

ENV ELAN_HOME=/opt/elan \
    PATH=/opt/elan/bin:${PATH}

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        libgmp10 \
        libuv1t64 \
        python3 \
        zstd \
    && rm -rf /var/lib/apt/lists/* \
    && gmp="$(dpkg-query --showformat '${Version}' --show libgmp10)" \
    && dpkg --compare-versions "$gmp" ge 2:6.3.0 \
    && printf 'libgmp10 %s\n' "$gmp"

COPY --from=lean /opt/lean/${LEAN_FORK_COMMIT} /opt/lean/${LEAN_FORK_COMMIT}

RUN curl --fail --location --silent --show-error --output /tmp/elan.tar.gz \
        "https://github.com/leanprover/elan/releases/download/v${ELAN_VERSION}/elan-x86_64-unknown-linux-gnu.tar.gz" \
    && echo "${ELAN_SHA256}  /tmp/elan.tar.gz" | sha256sum --check \
    && mkdir /tmp/elan \
    && tar --extract --gzip --file /tmp/elan.tar.gz --directory /tmp/elan \
    && /tmp/elan/elan-init -y --no-modify-path --default-toolchain none \
    && rm -rf /tmp/elan /tmp/elan.tar.gz \
    && elan toolchain link "${LEAN_TOOLCHAIN}" "/opt/lean/${LEAN_FORK_COMMIT}" \
    && elan default "${LEAN_TOOLCHAIN}" \
    && test "$(lean --githash)" = "${LEAN_FORK_COMMIT}" \
    && lean --version

WORKDIR /src
COPY packages/orchestration/go.mod packages/orchestration/go.sum ./
RUN go mod download
COPY packages/orchestration/ ./
RUN go build -o /usr/local/bin/declindex ./cmd/declindex \
    && go build -o /usr/local/bin/ledger ./cmd/ledger
