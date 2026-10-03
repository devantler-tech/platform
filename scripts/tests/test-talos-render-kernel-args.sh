#!/usr/bin/env bash
# Execute CI's actual render step with the installed, pinned talosctl. This
# generates and validates files offline; it starts no cluster and has no secrets.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
workflow=${1:-"$root/.github/workflows/ci.yaml"}
talos_bin=$(command -v "${TALOSCTL_BIN:-talosctl}")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/base/.github/workflows" "$work/base/.github/actions/deploy-prod"
cp "$root/.github/workflows/ci.yaml" "$root/.github/workflows/cd.yaml" "$work/base/.github/workflows/"
cp "$root/.github/actions/deploy-prod/action.yml" "$work/base/.github/actions/deploy-prod/"
cp "$root/ksail.prod.yaml" "$root/ksail.yaml" "$work/base/"
cp -R "$root/talos" "$root/talos-local" "$work/base/"
yq -r '.jobs.validate-talos.steps[] | select(.name == "✅ Render + validate patched machine configs") | .run' "$workflow" > "$work/render.sh"
[[ -s "$work/render.sh" ]] || { echo 'FAIL: actual CI render step absent'; exit 1; }
go build -o "$work/fold" "$root/scripts/reconcile-talos-kernel-args"
export FOLD_BINARY="$work/fold" REAL_TALOSCTL="$talos_bin"
cat > "$work/bin/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == run && "$2" == ./scripts/reconcile-talos-kernel-args ]] || exit 1
shift 2
exec "$FOLD_BINARY" "$@"
SH
cat > "$work/bin/talosctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == validate ]]; then
  config='' mode=''
  for ((i=1;i<=$#;i++)); do
    case "${!i}" in
      -c) j=$((i+1)); config="${!j}" ;;
      -m) j=$((i+1)); mode="${!j}" ;;
    esac
  done
  cp "$config" "$OBSERVED/$mode-$(basename "$config")"
fi
exec "$REAL_TALOSCTL" "$@"
SH
chmod +x "$work/bin/go" "$work/bin/talosctl"
export PATH="$work/bin:$PATH"

# A false setting in the input must become true only on production's extension
# path, on both roles. Local has no extensions and must preserve false + args.
cat > "$work/base/talos/cluster/zz-fold-proof.yaml" <<'YAML'
machine:
  install:
    grubUseUKICmdline: false
YAML
cp "$work/base/talos/cluster/zz-fold-proof.yaml" "$work/base/talos-local/cluster/zz-fold-proof.yaml"
cat >> "$work/base/talos-local/cluster/zz-fold-proof.yaml" <<'YAML'
    extraKernelArgs:
      - local-proof=unchanged
YAML

cp -R "$work/base" "$work/positive"
mkdir "$work/positive-observed"
export OBSERVED="$work/positive-observed"
if ! (cd "$work/positive" && bash "$work/render.sh") > "$work/positive.log" 2>&1; then
  echo 'FAIL: actual positive render step failed; inspect the offline validation log'
  tail -n 12 "$work/positive.log"
  exit 1
fi
for role in control-planes workers; do
  prod="$OBSERVED/cloud-$role-patched.yaml"
  local_config="$OBSERVED/container-$role-patched.yaml"
  [[ $(yq 'select(.version == "v1alpha1") | .machine.install.grubUseUKICmdline' "$prod") == true ]] || { echo 'FAIL: production UKI was not pinned'; exit 1; }
  [[ $(yq 'select(.version == "v1alpha1") | .machine.install | has("extraKernelArgs")' "$prod") == false ]] || { echo 'FAIL: production arguments were not folded'; exit 1; }
  [[ $(yq 'select(.version == "v1alpha1") | .machine.install.grubUseUKICmdline' "$local_config") == false ]] || { echo 'FAIL: local no-extension setting changed'; exit 1; }
  [[ $(yq 'select(.version == "v1alpha1") | .machine.install.extraKernelArgs[0]' "$local_config") == local-proof=unchanged ]] || { echo 'FAIL: local no-extension arguments changed'; exit 1; }
done
echo 'PASS: actual CI caller folds both production roles and preserves both local roles'

# KSail's explicit schematic selection bypasses the extension fold even when
# extensions remain configured. A whitespace-only ID still permits the fold.
for selection in explicit blank; do
  case_dir="$work/schematic-$selection"
  cp -R "$work/base" "$case_dir"
  mkdir "$case_dir/observed"
  export OBSERVED="$case_dir/observed"
  case "$selection" in
    explicit) yq -i '.spec.cluster.talos.schematicId = " explicit-proof "' "$case_dir/ksail.prod.yaml" ;;
    blank) yq -i '.spec.cluster.talos.schematicId = "   "' "$case_dir/ksail.prod.yaml" ;;
  esac
  cat >> "$case_dir/talos/cluster/zz-fold-proof.yaml" <<'YAML'
    extraKernelArgs:
      - schematic-proof=unchanged
YAML
  if ! (cd "$case_dir" && bash "$work/render.sh") > "$case_dir/result.log" 2>&1; then
    echo "FAIL: actual $selection schematic render step failed"
    tail -n 12 "$case_dir/result.log"
    exit 1
  fi
  for role in control-planes workers; do
    prod="$OBSERVED/cloud-$role-patched.yaml"
    if [[ "$selection" == explicit ]]; then
      [[ $(yq 'select(.version == "v1alpha1") | .machine.install.grubUseUKICmdline' "$prod") == false ]] || { echo 'FAIL: explicit schematic changed UKI'; exit 1; }
      [[ $(yq 'select(.version == "v1alpha1") | .machine.install.extraKernelArgs | contains(["schematic-proof=unchanged"])' "$prod") == true ]] || { echo 'FAIL: explicit schematic folded arguments'; exit 1; }
    else
      [[ $(yq 'select(.version == "v1alpha1") | .machine.install.grubUseUKICmdline' "$prod") == true ]] || { echo 'FAIL: blank schematic suppressed UKI'; exit 1; }
      [[ $(yq 'select(.version == "v1alpha1") | .machine.install | has("extraKernelArgs")' "$prod") == false ]] || { echo 'FAIL: blank schematic suppressed argument fold'; exit 1; }
    fi
  done
  echo "PASS: actual CI caller respects $selection schematic selection for both production roles"
done

for overlay in talos talos-local; do
  for role in control-planes workers; do
    case_dir="$work/invalid-$overlay-$role"
    cp -R "$work/base" "$case_dir"
    mkdir "$case_dir/observed"
    export OBSERVED="$case_dir/observed"
    # This is unrelated to kernel arguments. The fold must never turn an
    # invalid machine type into a valid config, in either role or environment.
    cat > "$case_dir/$overlay/$role/zz-invalid-proof.yaml" <<'YAML'
machine:
  type: invalid-machine-type
YAML
    if (cd "$case_dir" && bash "$work/render.sh") > "$case_dir/result.log" 2>&1; then
      echo "FAIL: invalid $overlay/$role patch passed the actual CI caller"
      exit 1
    fi
    if ! grep -q 'invalid-machine-type' "$case_dir/result.log"; then
      echo "FAIL: $overlay/$role failed for a reason unrelated to the negative patch"
      tail -n 12 "$case_dir/result.log"
      exit 1
    fi
    echo "PASS: actual CI caller rejects invalid $overlay/$role patch"
  done
done
