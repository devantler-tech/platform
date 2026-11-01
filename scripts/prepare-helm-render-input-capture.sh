#!/usr/bin/env bash
# Prepare an identity post-renderer inside a caller-owned private Helm directory.
# The caller supplies HELM_GUARD_CAPTURE for each audited SDK template invocation.
set -euo pipefail
if [[ $# != 1 || -z ${1:-} || $1 != /* ]]; then
  printf '%s\n' 'usage: prepare-helm-render-input-capture.sh <private-absolute-plugin-directory>' >&2
  exit 2
fi
capture_plugin="$1/guard-input-capture"
mkdir -p "$capture_plugin"
cat >"$capture_plugin/plugin.yaml" <<'YAML'
apiVersion: v1
type: postrenderer/v1
name: guard-input-capture
version: 0.1.0
runtime: subprocess
runtimeConfig:
  platformCommand:
    - command: ${HELM_PLUGIN_DIR}/capture.sh
YAML
cat >"$capture_plugin/capture.sh" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
tee "${HELM_GUARD_CAPTURE:?capture path required}"
BASH
chmod +x "$capture_plugin/capture.sh"
