# Local reproduction image for the e2e driver (W3-E2E10: all ten hosts).
#
# CI runs the same driver in a plain `ubuntu-latest`/`macos-latest` job with
# setup-go and the same install steps (see .github/workflows/e2e.yml); this
# image exists so a developer can run one matrix leg locally with Docker (see
# run.sh). Versions are the same pins the workflow passes; bump them together
# (OQ-T1.13.1).
#
# Node 24 is the floor: dsh (@deepseek-ai/dsh) requires Node >= 24.
FROM node:24-bookworm

ARG CLAUDE_VERSION=2.1.284
ARG CODEX_VERSION=0.158.0
ARG GEMINI_VERSION=0.46.0
ARG KILO_VERSION=7.8.1
ARG PI_VERSION=0.74.2
ARG DSH_VERSION=0.1.7-rc.2
ARG OMP_VERSION=18.4.2
# opencode 2.x is not on npm (the npm package `opencode-ai` is the 1.x line):
# it ships as a release archive from opencode.ai, the same file the
# anomalyco/tap/opencode-v2 formula installs. The digests are the formula's.
ARG OPENCODE_VERSION=2.0.18
ARG OPENCODE_SHA_ARM64=f46253f0ff5eff0c1751d3b734ede55cc60d932d60af81d061d03745f9998082
ARG OPENCODE_SHA_X64=5db6a93cadec1704b150e3cc807b95e0d6366651c906b0003fb994fba05b7b40
# omp's launcher is `#!/usr/bin/env bun`; the runtime is its own npm package.
ARG BUN_VERSION=latest

# Go for building/driving the repo; node:24-bookworm has everything else.
COPY --from=golang:1.27-bookworm /usr/local/go /usr/local/go

ENV PATH="/usr/local/go/bin:/root/.local/bin:${PATH}" \
    GOTOOLCHAIN=local \
    GOPATH=/go

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl unzip \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g \
      "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
      "@google/gemini-cli@${GEMINI_VERSION}" \
      "@kilocode/cli@${KILO_VERSION}" \
      "@earendil-works/pi-coding-agent@${PI_VERSION}" \
      "@deepseek-ai/dsh@${DSH_VERSION}" \
      "@oh-my-pi/pi-coding-agent@${OMP_VERSION}" \
      "bun@${BUN_VERSION}"

# opencode 2.x from the release archive, digest-checked.
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      arm64) asset="opencode-linux-arm64.tar.gz"; sha="${OPENCODE_SHA_ARM64}" ;; \
      amd64) asset="opencode-linux-x64-baseline.tar.gz"; sha="${OPENCODE_SHA_X64}" ;; \
      *) echo "no opencode asset for $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/opencode.tgz "https://opencode.ai/files/bin/${OPENCODE_VERSION}/${asset}"; \
    echo "${sha}  /tmp/opencode.tgz" | sha256sum -c -; \
    tar -xzf /tmp/opencode.tgz -C /tmp; \
    install -m 0755 /tmp/opencode /usr/local/bin/opencode; \
    rm -f /tmp/opencode.tgz /tmp/opencode

# cursor-agent ships from cursor.com/install (a self-updating binary), not from
# npm; the installer is the documented user path. It links the binary as
# `agent`, while every documented invocation (and the adapter's host-CLI name)
# is `cursor-agent`, so the second name is linked here.
RUN curl -fsSL https://cursor.com/install | bash \
    && ln -sf /root/.local/bin/agent /root/.local/bin/cursor-agent \
    && cursor-agent --version

WORKDIR /src
