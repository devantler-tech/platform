#!/usr/bin/env bash
# Guards the Devantler Tech theme on Dex's sign-in pages.
#
# Dex reads its theme from `<frontend.dir>/themes/<frontend.theme>/` and never
# checks that the directory exists. A theme name the mount does not provide, or
# a mount at another path, starts Dex normally and serves every sign-in page
# without its stylesheet or logo. The HelmRelease values and the ConfigMap are
# each valid on their own, so schema validation cannot see the mismatch.
#
# In each provider's rendered controllers layer, the tree Flux applies with the
# HelmRelease, this checks that:
#   - the dex HelmRelease names a frontend theme and an absolute frontend dir;
#   - exactly one volume is mounted at <dir>/themes/<theme>, as a whole
#     directory (no subPath), from a ConfigMap that maps every key (no items);
#   - that ConfigMap is rendered in the dex namespace and holds a non-empty
#     styles.css and favicon.png, which every page links to, and the file that
#     frontend.logoURL names under theme/.
# A logoURL outside theme/ would load the logo from somewhere other than this
# cluster, so it is refused.
#
# Every check is first run against a broken copy of the real render, to prove
# it fails for the reason it names.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly repo_root

readonly providers=(hetzner docker)
readonly theme_name_pattern='^[a-z0-9][a-z0-9-]*$'

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

fail() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

readonly dex_release='select(.kind == "HelmRelease" and .metadata.name == "dex" and .metadata.namespace == "dex")'

# read_yaml <expression> <file>: one yq read, or a printed reason and status 1.
# Reads are checked one by one so that a failed read is never taken for an
# absent field.
read_yaml() {
  if ! yq eval -N "$1" "$2" 2>"${workdir}/yq.err"; then
    printf 'could not read %s: %s\n' "$2" "$(head -3 "${workdir}/yq.err")"
    return 1
  fi
}

read_all() {
  if ! yq eval-all -N "$1" "$2" 2>"${workdir}/yq.err"; then
    printf 'could not read %s: %s\n' "$2" "$(head -3 "${workdir}/yq.err")"
    return 1
  fi
}

# check_theme <rendered.yaml>: prints the first problem found and returns 1, or
# returns 0 when the theme Dex is told to use is the directory that is mounted.
check_theme() {
  local rendered="$1"
  local count values theme dir mount_path volume sub_path cm_name items key size logo logo_file

  count="$(read_all "[${dex_release}] | length" "${rendered}")" || { printf '%s\n' "${count}"; return 1; }
  if [[ "${count}" != 1 ]]; then
    printf 'renders %s dex HelmReleases in namespace dex instead of one\n' "${count:-no}"
    return 1
  fi
  values="${workdir}/dex-values.yaml"
  read_yaml "${dex_release} | .spec.values" "${rendered}" >"${values}" || { cat "${values}"; return 1; }

  theme="$(read_yaml '.config.frontend.theme // ""' "${values}")" || { printf '%s\n' "${theme}"; return 1; }
  if [[ -z "${theme}" ]]; then
    printf 'the dex HelmRelease names no config.frontend.theme, so Dex shows its built-in light theme\n'
    return 1
  fi
  if ! [[ "${theme}" =~ ${theme_name_pattern} ]]; then
    printf "config.frontend.theme '%s' is not a single directory name\n" "${theme}"
    return 1
  fi

  dir="$(read_yaml '.config.frontend.dir // ""' "${values}")" || { printf '%s\n' "${dir}"; return 1; }
  if [[ "${dir}" != /* ]]; then
    printf 'the dex HelmRelease names no absolute config.frontend.dir, so Dex serves its built-in files and never sees a mounted theme\n'
    return 1
  fi
  mount_path="${dir%/}/themes/${theme}"

  count="$(MOUNT="${mount_path}" read_yaml '[.volumeMounts[]? | select((.mountPath | sub("/$"; "")) == strenv(MOUNT))] | length' "${values}")" ||
    { printf '%s\n' "${count}"; return 1; }
  if [[ "${count}" != 1 ]]; then
    printf "the dex HelmRelease mounts %s volumes at %s, where Dex looks for theme '%s'; it needs exactly one\n" "${count}" "${mount_path}" "${theme}"
    return 1
  fi
  volume="$(MOUNT="${mount_path}" read_yaml '.volumeMounts[] | select((.mountPath | sub("/$"; "")) == strenv(MOUNT)) | .name // ""' "${values}")" ||
    { printf '%s\n' "${volume}"; return 1; }
  sub_path="$(MOUNT="${mount_path}" read_yaml '.volumeMounts[] | select((.mountPath | sub("/$"; "")) == strenv(MOUNT)) | (.subPath // "") + (.subPathExpr // "")' "${values}")" ||
    { printf '%s\n' "${sub_path}"; return 1; }
  if [[ -n "${sub_path}" ]]; then
    printf 'the theme is mounted at %s with a subPath; mount the whole ConfigMap so the theme is a directory\n' "${mount_path}"
    return 1
  fi

  count="$(VOL="${volume}" read_yaml '[.volumes[]? | select(.name == strenv(VOL))] | length' "${values}")" || { printf '%s\n' "${count}"; return 1; }
  if [[ -z "${volume}" || "${count}" != 1 ]]; then
    printf "the volume '%s' mounted at %s is defined %s times in the dex HelmRelease instead of once\n" "${volume}" "${mount_path}" "${count}"
    return 1
  fi
  cm_name="$(VOL="${volume}" read_yaml '.volumes[] | select(.name == strenv(VOL)) | .configMap.name // ""' "${values}")" ||
    { printf '%s\n' "${cm_name}"; return 1; }
  if [[ -z "${cm_name}" ]]; then
    printf "the volume '%s' mounted at %s is not a ConfigMap volume\n" "${volume}" "${mount_path}"
    return 1
  fi
  items="$(VOL="${volume}" read_yaml '.volumes[] | select(.name == strenv(VOL)) | .configMap.items // [] | length' "${values}")" ||
    { printf '%s\n' "${items}"; return 1; }
  if [[ "${items}" != 0 ]]; then
    printf "the volume '%s' selects ConfigMap keys with items; mount every key, the way this check reads them\n" "${volume}"
    return 1
  fi

  local -r configmap="select(.kind == \"ConfigMap\" and .metadata.name == strenv(CM) and .metadata.namespace == \"dex\")"
  count="$(CM="${cm_name}" read_all "[${configmap}] | length" "${rendered}")" || { printf '%s\n' "${count}"; return 1; }
  if [[ "${count}" != 1 ]]; then
    printf "the ConfigMap '%s' that the theme is mounted from is rendered %s times in namespace dex in this layer instead of once\n" "${cm_name}" "${count}"
    return 1
  fi

  logo="$(read_yaml '.config.frontend.logoURL // ""' "${values}")" || { printf '%s\n' "${logo}"; return 1; }
  if [[ -z "${logo}" ]]; then
    logo_file=logo.png # Dex's default logoURL is theme/logo.png.
  elif [[ "${logo}" =~ ^theme/([A-Za-z0-9._-]+)$ ]]; then
    logo_file="${BASH_REMATCH[1]}"
  else
    printf "config.frontend.logoURL '%s' is outside the theme; name a file in it as theme/<file> so the logo is served from this cluster\n" "${logo}"
    return 1
  fi

  # styles.css is text, so it must be in data; the images may be in either map.
  size="$(CM="${cm_name}" read_yaml "${configmap} | .data[\"styles.css\"] // \"\" | length" "${rendered}")" ||
    { printf '%s\n' "${size}"; return 1; }
  if [[ -z "${size}" || "${size}" == 0 ]]; then
    printf "the ConfigMap '%s' has no styles.css in data, so the pages render unstyled\n" "${cm_name}"
    return 1
  fi
  for key in favicon.png "${logo_file}"; do
    size="$(CM="${cm_name}" KEY="${key}" read_yaml "${configmap} | (.data[strenv(KEY)] // .binaryData[strenv(KEY)] // \"\") | length" "${rendered}")" ||
      { printf '%s\n' "${size}"; return 1; }
    if [[ -z "${size}" || "${size}" == 0 ]]; then
      printf "the ConfigMap '%s' has no %s, which the pages link to as theme/%s\n" "${cm_name}" "${key}" "${key}"
      return 1
    fi
  done
}

# expect_failure <control> <file> <reason>: the check must fail, and say why.
expect_failure() {
  local out
  if out="$(check_theme "$2")"; then
    fail "control '$1': the check passed a broken theme"
  fi
  [[ "${out}" == *"$3"* ]] ||
    fail "control '$1' failed for the wrong reason (wanted '$3'): ${out}"
}

# --- Render each provider's controllers layer ---------------------------------
for provider in "${providers[@]}"; do
  dir="${repo_root}/k8s/providers/${provider}/infrastructure/controllers"
  kubectl kustomize "${dir}" >"${workdir}/${provider}.yaml" 2>"${workdir}/render.err" ||
    fail "k8s/providers/${provider}/infrastructure/controllers failed to build: $(tail -5 "${workdir}/render.err")"
done

# --- Prove each check fires, on broken copies of the production render --------
real="${workdir}/hetzner.yaml"
broken="${workdir}/broken.yaml"
readonly hr_values='select(.kind == "HelmRelease" and .metadata.name == "dex") | .spec.values'
readonly is_theme_cm='.kind == "ConfigMap" and .metadata.name == "dex-theme"'
readonly theme_cm="select(${is_theme_cm})"

yq eval "(${hr_values} | .config.frontend.theme) = \"renamed\"" "${real}" >"${broken}"
expect_failure 'theme renamed' "${broken}" 'mounts 0 volumes at'

yq eval "del(${hr_values} | .config.frontend.dir)" "${real}" >"${broken}"
expect_failure 'no frontend dir' "${broken}" 'no absolute config.frontend.dir'

yq eval "(${hr_values} | .volumeMounts[] | select(.name == \"theme\") | .subPath) = \"styles.css\"" "${real}" >"${broken}"
expect_failure 'subPath mount' "${broken}" 'with a subPath'

yq eval "select((${is_theme_cm}) | not)" "${real}" >"${broken}"
expect_failure 'ConfigMap missing' "${broken}" 'rendered 0 times'

yq eval "del(${theme_cm} | .data[\"styles.css\"])" "${real}" >"${broken}"
expect_failure 'no stylesheet' "${broken}" 'no styles.css'

yq eval "del(${theme_cm} | .binaryData[\"favicon.png\"])" "${real}" >"${broken}"
expect_failure 'no favicon' "${broken}" 'no favicon.png'

yq eval "(${hr_values} | .config.frontend.logoURL) = \"theme/logo.png\"" "${real}" >"${broken}"
expect_failure 'logo not in theme' "${broken}" 'no logo.png'

yq eval "(${hr_values} | .config.frontend.logoURL) = \"https://example.invalid/logo.png\"" "${real}" >"${broken}"
expect_failure 'hot-linked logo' "${broken}" 'is outside the theme'

# --- Check the committed tree --------------------------------------------------
for provider in "${providers[@]}"; do
  out="$(check_theme "${workdir}/${provider}.yaml")" ||
    fail "${provider}: ${out}. The Dex theme is configured in k8s/bases/infrastructure/controllers/dex/ (helm-release.yaml and config-map.yaml)"
done

printf 'PASS: on %s, Dex is told to use the theme directory its ConfigMap is mounted at, and that theme holds every file the pages link to\n' \
  "${providers[*]}"
