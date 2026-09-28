#!/usr/bin/env bash
# Local reproduction helper for the T1.13 e2e driver.
#
#   ./e2e/run.sh                 # build the image and run the claude leg
#   ./e2e/run.sh gemini          # one host: claude|codex|gemini|omp
#   ./e2e/run.sh claude noimage  # skip the image build (image already local)
#   ./e2e/run.sh claude negative # negative leg (no host CLI on PATH)
#   ./e2e/run.sh claude check    # preflight only: assert the expected tests exist
#
# E2E_LOCAL=1 runs the commands on this machine instead of inside the image
# (Go and, for a real leg, the host CLI must be installed; see README.md).
#
# CI does the same with node:22-bookworm + setup-go; the image only bundles
# node, Go and the three pinned agents for offline/local convenience.
#
# Every mode first runs a preflight that lists the tests selected by the leg's
# -run pattern and aborts with exit 1 unless the expected scenarios are there:
# a renamed or deleted test used to make `go test` print "[no tests to run]"
# and exit 0, turning the local runner into a silent green (T1.13-verify-1 F1).
set -euo pipefail

host="${1:-claude}"
mode="${2:-}"

usage() {
  echo "usage: $0 [claude|codex|gemini|omp] [noimage|negative|check]" >&2
  exit 2
}

case "$host" in
  claude | codex | gemini | omp) ;;
  *) usage ;;
esac

case "$mode" in
  "" | noimage | negative | check) ;;
  *) usage ;;
esac

repo="$(cd "$(dirname "$0")/.." && pwd)"
image="verger-e2e:local"

if [[ "$mode" != "noimage" && "${E2E_LOCAL:-}" != "1" ]]; then
  docker build -t "$image" "$repo/e2e"
fi

# The -run pattern and the scenario functions each leg must select. Both are
# asserted by the preflight, so the pattern and the names cannot drift apart
# silently again.
if [[ "$mode" == "negative" || "$mode" == "check" ]]; then
  neg_pattern='TestE2ENegative'
  neg_expected=(TestE2ENegative)
fi

if [[ "$mode" != "negative" ]]; then
  pattern='TestE2E(Local|Remote)'
  expected=(TestE2ELocalFixtureScenario TestE2ERemoteArchiveScenario)
  timeout=25m
else
  pattern="$neg_pattern"
  expected=("${neg_expected[@]}")
  timeout=15m
fi

local_env=(E2E=1 E2E_HOST="$host")
docker_env=(-e E2E=1 -e E2E_HOST="$host")

if [[ "$mode" == "negative" ]]; then
  local_env+=(E2E_NEGATIVE=1)
  docker_env+=(-e E2E_NEGATIVE=1)
fi

# run_in_env executes one command inside the image, or on this machine with
# E2E_LOCAL=1 (always from the repository root, so relative paths hold).
run_in_env() {
  if [[ "${E2E_LOCAL:-}" == "1" ]]; then
    ( cd "$repo" && env "${local_env[@]}" "$@" )
  else
    docker run --rm "${docker_env[@]}" -v "$repo:/src" -w /src "$image" "$@"
  fi
}

# preflight fails (exit 1) unless `-list` prints every expected scenario of the
# given pattern.
preflight() {
  local pattern="$1"
  shift

  local listed
  listed="$(run_in_env go test -mod=vendor -tags e2e -list "$pattern" ./e2e/...)"

  local want missing=()
  for want in "$@"; do
    if ! printf '%s\n' "$listed" | grep -qx "$want"; then
      missing+=("$want")
    fi
  done

  if (( ${#missing[@]} > 0 )); then
    {
      echo "e2e preflight failed: -run '$pattern' does not select: ${missing[*]}"
      echo "Refusing to run: a stale pattern would exit 0 with [no tests to run]."
      echo "Selected tests:"
      if [[ -z "$listed" ]]; then
        echo "  (none)"
      else
        printf '%s\n' "$listed" | sed 's/^/  /'
      fi
    } >&2

    return 1
  fi
}

if [[ "$mode" == "check" ]]; then
  preflight "$pattern" "${expected[@]}"
  preflight "$neg_pattern" "${neg_expected[@]}"
  echo "e2e preflight ok: '$pattern' selects ${expected[*]}; '$neg_pattern' selects ${neg_expected[*]}"

  exit 0
fi

preflight "$pattern" "${expected[@]}"

run_in_env go test -mod=vendor -tags e2e -count=1 -timeout "$timeout" -run "$pattern" -v ./e2e/...
