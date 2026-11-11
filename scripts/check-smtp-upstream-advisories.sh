#!/usr/bin/env bash
# Local module replacements lose their upstream version in govulncheck's SBOM.
# Keep a separate, version-bound module scan of the copied source's upstream base.
set -euo pipefail

[[ $# == 1 && -x $1 ]] || {
  echo 'usage: check-smtp-upstream-advisories.sh SCANNER' >&2
  exit 2
}
scanner=$1
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
metadata="$root/third_party/go-smtp/UPSTREAM.json"
module=$(jq -er '.module | select(. == "github.com/emersion/go-smtp")' "$metadata")
version=$(jq -er '.version | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' "$metadata")
expected_sum=$(jq -er '.sum | select(test("^h1:[A-Za-z0-9+/]{43}=$"))' "$metadata")
expected_mod_sum=$(jq -er '.goModSum | select(test("^h1:[A-Za-z0-9+/]{43}=$"))' "$metadata")
work=$(mktemp -d "${TMPDIR:-/tmp}/smtp-upstream-advisories.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
export GOTOOLCHAIN=local GOWORK=off
cd "$work"
go mod init example.invalid/smtp-upstream-advisories >/dev/null
go mod edit "-require=$module@$version"
printf '%s\n' 'package main' 'import _ "github.com/emersion/go-smtp"' 'func main() {}' >main.go
go mod tidy
go mod download -json "$module@$version" >origin.json
jq -e --arg sum "$expected_sum" --arg mod_sum "$expected_mod_sum" \
  '.Sum == $sum and .GoModSum == $mod_sum' origin.json >/dev/null
"$scanner" -json -scan=module >report.json
jq -s -e --arg module "$module" --arg version "$version" '
  [.[] | select(has("SBOM"))] as $sboms |
  ($sboms | length) == 1 and
  any($sboms[0].SBOM.modules[]; .path == $module and .version == $version) and
  all(.[]; .finding? == null)
' report.json >/dev/null
printf 'SMTP_UPSTREAM_ADVISORIES=PASS module=%s version=%s\n' "$module" "$version"
