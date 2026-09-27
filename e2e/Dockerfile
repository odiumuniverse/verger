# Local reproduction image for the T1.13 e2e driver.
#
# CI runs the same driver in a plain `node:22-bookworm` job with setup-go; this
# image exists so a developer can run one matrix leg locally with Docker (see
# run.sh). Versions are the same pins the workflow passes; bump them together
# (OQ-T1.13.1).
FROM node:22-bookworm

ARG CLAUDE_VERSION=2.1.283
ARG CODEX_VERSION=0.157.1
ARG GEMINI_VERSION=0.61.0

# Go for building/driving the repo; node:22-bookworm has everything else.
COPY --from=golang:1.27-bookworm /usr/local/go /usr/local/go

ENV PATH="/usr/local/go/bin:${PATH}" \
    GOTOOLCHAIN=local \
    GOPATH=/go

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g \
      "@anthropic-ai/claude-code@${CLAUDE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
      "@google/gemini-cli@${GEMINI_VERSION}"

WORKDIR /src
