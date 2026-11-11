#!/usr/bin/env bash
# Prove the kagent lane's harness image loads the whole agentic-engineering plugin.
#
# kagent's Claude harness cannot load the plugin whole yet (kagent-dev/kagent#3388), so the image
# bakes the plugin in at a pinned agent-plugins commit and puts a `claude` wrapper where the harness
# finds Claude Code. This runs the real wrapper against a stub Claude Code: a prompt run (-p) gets
# the plugin once, every other call passes through unchanged, and arguments, input and exit status
# reach Claude Code intact. It also checks that the Dockerfile puts the plugin and Claude Code where
# the wrapper expects them, pins the plugin by commit, and that Renovate moves that commit.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
context="${root}/scripts/kagent-claude-harness"
dockerfile="${context}/Dockerfile"
wrapper="${context}/claude"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: kagent Claude harness wrapper: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: kagent Claude harness wrapper: %s\n' "$1" >&2
  exit 1
}
for tool in jq sh; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

real=/usr/local/libexec/claude-code/claude
plugin=/opt/agent-plugins/agentic-engineering

# The Dockerfile and the wrapper must agree on where Claude Code and the plugin live.
grep -Fq "exec ${real} \"\$@\" --plugin-dir ${plugin}" "${wrapper}" ||
  fail "the wrapper does not run ${real} with --plugin-dir ${plugin}"
grep -Fq "mv /usr/local/bin/claude ${real}" "${dockerfile}" ||
  fail "the Dockerfile does not move Claude Code to ${real}"
grep -Fq 'COPY --chmod=0755 claude /usr/local/bin/claude' "${dockerfile}" ||
  fail 'the Dockerfile does not put the wrapper where the harness finds Claude Code'
grep -Fq "COPY --from=plugin /tmp/src/plugins/agentic-engineering ${plugin}" "${dockerfile}" ||
  fail "the Dockerfile does not copy the plugin to ${plugin}"
# One base image for both stages, pinned by digest, and the plugin pinned by a full commit.
bases="$(sed -n 's/^FROM \([^ ]*\).*/\1/p' "${dockerfile}" | sort -u)"
[[ "$(wc -l <<<"${bases}")" -eq 1 && "${bases}" =~ @sha256:[0-9a-f]{64}$ ]] ||
  fail "the stages do not share one digest-pinned base: ${bases}"
grep -Eq '^ARG AGENT_PLUGINS_COMMIT=[0-9a-f]{40}$' "${dockerfile}" ||
  fail 'the plugin is not pinned by a full commit'
# shellcheck disable=SC2016 # a literal line of the Dockerfile, not an expansion
grep -Fq 'test "$(git -C /tmp/src rev-parse HEAD)" = "${AGENT_PLUGINS_COMMIT}"' "${dockerfile}" ||
  fail 'the build does not check it fetched the pinned commit'
manager="$(jq -c '[.customManagers[] | select(.depNameTemplate == "devantler-tech/agent-plugins")
  | select(.datasourceTemplate == "github-digest" and .currentValueTemplate == "main")
  | select(any(.managerFilePatterns[]; . == "/^scripts/kagent-claude-harness/Dockerfile$/"))
  | select(any(.matchStrings[]; startswith("ARG AGENT_PLUGINS_COMMIT=")))] | length' \
  "${root}/.github/renovate.json")"
[[ "${manager}" == 1 ]] || fail 'Renovate does not move the plugin commit'
# The lane's Harness runs this image, pinned by digest as kagent requires; the built-in image has
# neither the tools nor the whole plugin.
command -v yq >/dev/null || fail 'yq is required'
harness_image="$(yq '.spec.workload.image' "${root}/k8s/providers/hetzner/apps/agentic-engineer/harness.yaml")"
[[ "${harness_image}" =~ ^ghcr\.io/devantler-tech/platform-kagent-claude-harness@sha256:[0-9a-f]{64}$ ]] ||
  fail "the lane's Harness does not run this image by digest: ${harness_image}"
grep -Fq 'IMAGE: ghcr.io/devantler-tech/platform-kagent-claude-harness' "${root}/.github/workflows/publish-kagent-claude-harness.yaml" ||
  fail 'the Harness image is not the one the publish workflow builds'

# The stub Claude Code records its arguments one per line, then its input, and exits with $STUB_EXIT.
cat >"${work}/real-claude" <<'STUB'
#!/bin/sh
printf '%s\n' "$@" >"${STUB_DIR:?}/args"
cat >"${STUB_DIR}/stdin"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "${work}/real-claude"
sed "s|${real}|${work}/real-claude|g" "${wrapper}" >"${work}/claude"
grep -q "${work}/real-claude" "${work}/claude" || fail 'the stub could not stand in for Claude Code'

# run <name> <input> <args...>: runs the wrapper and prints its exit status.
run() {
  local name="$1" input="$2" status=0
  shift 2
  mkdir -p "${work}/${name}"
  printf '%s' "${input}" | STUB_DIR="${work}/${name}" sh "${work}/claude" "$@" || status=$?
  printf '%s' "${status}"
}
# args <name>: the arguments Claude Code received, joined by |.
args() { tr '\n' '|' <"${work}/$1/args"; }

# A prompt run gets the plugin once, after the harness's own arguments, with its input intact.
[[ "$(run prompt '{"type":"user"}' -p --input-format stream-json --plugin-dir /data/generated/plugin \
  --resume abc)" == 0 ]] || fail 'a prompt run failed'
[[ "$(args prompt)" == "-p|--input-format|stream-json|--plugin-dir|/data/generated/plugin|--resume|abc|--plugin-dir|${plugin}|" ]] ||
  fail "a prompt run did not get the plugin exactly once: $(args prompt)"
[[ "$(cat "${work}/prompt/stdin")" == '{"type":"user"}' ]] || fail "a prompt run's input did not reach Claude Code"

# The harness's version probe, and any other call, passes through unchanged.
[[ "$(run version '' --version)" == 0 && "$(args version)" == '--version|' ]] ||
  fail "the version probe was changed: $(args version)"
[[ "$(run other '' --print-config)" == 0 && "$(args other)" == '--print-config|' ]] ||
  fail "a call without -p was changed: $(args other)"
# A value that merely contains -p is not a prompt run.
[[ "$(run lookalike '' --model x-p)" == 0 && "$(args lookalike)" == '--model|x-p|' ]] ||
  fail "an argument containing -p was taken for a prompt run: $(args lookalike)"

# Arguments with spaces reach Claude Code as one argument, and its exit status is the wrapper's.
[[ "$(run spaced '' -p --append-system-prompt-file '/data/a b.md')" == 0 ]] || fail 'a spaced argument failed'
[[ "$(args spaced)" == "-p|--append-system-prompt-file|/data/a b.md|--plugin-dir|${plugin}|" ]] ||
  fail "a spaced argument was split: $(args spaced)"
[[ "$(STUB_EXIT=3 run failing '' -p)" == 3 ]] || fail "Claude Code's exit status was lost"

grep -Fq 'bash scripts/tests/test-kagent-claude-harness-wrapper.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'kagent Claude harness wrapper: PASS\n'
