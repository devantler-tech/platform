#!/usr/bin/env bash
# The only credential-bearing wrapper; keep Kubernetes access in ephemeral RAM.
set +x
set -euo pipefail
umask 077
stage=dispatch
fail() { printf 'EMAIL_PROVIDER_SMTP=UNVERIFIED stage=%s\n' "$stage" >&2; exit 2; }
[[ $# == 0 || ( $# == 1 && $1 == --cleanup ) ]] || fail
[[ $GITHUB_EVENT_NAME == workflow_dispatch && $GITHUB_REF == refs/heads/main && $GITHUB_REPOSITORY == devantler-tech/platform && $GITHUB_ACTOR == devantler && $GITHUB_RUN_ATTEMPT == 1 && $RUNNER_ENVIRONMENT == github-hosted && $EMAIL_PROVIDER_CONFIRM == verify-email-provider-smtp ]] || fail
stage=private-workspace
[[ $(findmnt -n -o FSTYPE --target /dev/shm) == tmpfs ]] || fail
private_dir="$(mktemp -d /dev/shm/email-provider.XXXXXXXX)"
trap 'rm -rf "$private_dir"' EXIT
[[ -n $KUBE_CONFIG && ${#KUBE_CONFIG} -le 1048576 ]] || fail
export KUBECONFIG="$private_dir/kubeconfig"
printf '%s' "$KUBE_CONFIG" > "$KUBECONFIG"
unset KUBE_CONFIG
stage=stable-api
if ! ./scripts/use-prod-stable-api-endpoint.sh >/dev/null 2>&1; then fail; fi
unset HCLOUD_TOKEN
"$RUNNER_TEMP/verify-email-provider" "$@"
