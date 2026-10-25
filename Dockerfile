# Offload node image: the offload binary plus the toolchains your tests need.
# Runs as-is under Docker, and is the base for the E2B template and for
# microsandbox (see docs/backends.md). Trim or extend the toolchains to match
# your repos; build args set to "" skip one.
#
#   docker build -t offload-node .
#   docker build -t offload-node --build-arg RUST_VERSION= --build-arg NODE_VERSION= .   # Go only

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /offload ./cmd/offload

FROM ubuntu:24.04
ARG GO_VERSION=1.27.1
ARG NODE_VERSION=24.13.0
ARG RUST_VERSION=1.98.1
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -q && apt-get install -yq --no-install-recommends \
      git ca-certificates curl xz-utils build-essential pkg-config libssl-dev python3 python3-venv \
    && rm -rf /var/lib/apt/lists/*
# The state dir is world-writable so the node can run as any user (E2B and
# microsandbox pick their own); toolchains are installed system-wide for the same reason.
RUN useradd -m -s /bin/bash offload && mkdir -p /data/offload && chmod 1777 /data/offload
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:/usr/local/go/bin:$PATH
RUN arch="$(dpkg --print-architecture)"; set -eux; \
    if [ -n "$GO_VERSION" ]; then \
      curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" | tar -C /usr/local -xz; fi; \
    if [ -n "$NODE_VERSION" ]; then \
      narch="$([ "$arch" = amd64 ] && echo x64 || echo arm64)"; \
      curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${narch}.tar.xz" \
        | tar -C /usr/local --strip-components=1 -xJ; \
      npm i -g pnpm; fi; \
    if [ -n "$RUST_VERSION" ]; then \
      curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --default-toolchain "$RUST_VERSION" --profile minimal; \
      chmod -R a+w "$RUSTUP_HOME" "$CARGO_HOME"; fi
USER offload
COPY --from=build /offload /usr/local/bin/offload
VOLUME /data/offload
EXPOSE 7070
CMD ["offload", "node", "--listen", ":7070", "--root", "/data/offload"]
