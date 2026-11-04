#!/usr/bin/env bash
# The old-compiler control is data until an isolated scan/validation needs it.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
control=scripts/tests/fixtures/govulncheck-negative-control
fixture() {
  local destination="$1" name source
  mkdir -p "$destination/$control" "$destination/scripts"
  cp "$root/scripts/check-go-negative-control.sh" "$destination/scripts/check-go-negative-control.sh"
  for name in go.mod main.go; do
    source="$root/$control/$name.tmpl"
    if [ ! -f "$source" ]; then source="$root/$control/$name"; fi
    cp "$source" "$destination/$control/$name.tmpl"
  done
  git -C "$destination" init -q
  git -C "$destination" add -- "$control/go.mod.tmpl" "$control/main.go.tmpl" scripts/check-go-negative-control.sh
}
refuse() {
  if bash "$1/scripts/check-go-negative-control.sh" "${@:2}" >"$work/refusal.log" 2>&1; then
    echo 'FAIL: an unknown, altered or unsafe control was accepted' >&2
    exit 1
  fi
}
fixture "$work/valid"
bash "$work/valid/scripts/check-go-negative-control.sh" --check
bash "$work/valid/scripts/check-go-negative-control.sh" --materialize "$work/emitted" >"$work/emitted-path"
for name in go.mod main.go; do
  cmp "$work/valid/$control/$name.tmpl" "$work/emitted/$name"
done
refuse "$work/valid" --materialize "$work/emitted"
refuse "$work/valid" --materialize relative
refuse "$work/valid" --materialize "$work/valid/unexpected-module"
for mutation in source manifest orphan unknown real-source untracked-source untracked-hidden linked; do
  destination="$work/$mutation"
  fixture "$destination"
  case "$mutation" in
    source) printf '\n// altered\n' >>"$destination/$control/main.go.tmpl" ;;
    manifest) printf '\ntoolchain go1.27.3\n' >>"$destination/$control/go.mod.tmpl" ;;
    orphan) rm "$destination/$control/main.go.tmpl" ;;
    unknown)
      cp "$destination/$control/main.go.tmpl" "$destination/unknown.go.tmpl"
      git -C "$destination" add -- unknown.go.tmpl
      ;;
    real-source)
      cp "$destination/$control/main.go.tmpl" "$destination/$control/main.go"
      git -C "$destination" add -- "$control/main.go"
      ;;
    untracked-source)
      cp "$destination/$control/main.go.tmpl" "$destination/$control/main.go"
      ;;
    untracked-hidden)
      printf 'unexpected input\n' >"$destination/$control/.extra"
      ;;
    linked)
      mv "$destination/$control/main.go.tmpl" "$destination/source-data"
      ln -s "$destination/source-data" "$destination/$control/main.go.tmpl"
      ;;
  esac
  refuse "$destination" --materialize "$work/rejected-$mutation"
  [ ! -e "$work/rejected-$mutation" ] || {
    echo 'FAIL: rejected control left generated files' >&2
    exit 1
  }
done
ln -s "$work/emitted" "$work/linked-destination"
refuse "$work/valid" --materialize "$work/linked-destination"
cmp "$work/valid/$control/main.go.tmpl" "$work/emitted/main.go"
echo 'PASS: byte-identical materialization, complete template inventory and source/destination refusals'
