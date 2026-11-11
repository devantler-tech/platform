#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/smtp-advisory-controls.XXXXXX")
trap 'rm -rf -- "$fixture"' EXIT
mkdir "$fixture/bin"
export SMTP_ADVISORY_METADATA="$root/third_party/go-smtp/UPSTREAM.json"
cat >"$fixture/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'list -mod=readonly')
    [[ $SMTP_ADVISORY_CASE != root-unreadable ]] || exit 3
    version=$(jq -r .version "$SMTP_ADVISORY_METADATA")
    replacement=./third_party/go-smtp
    [[ $SMTP_ADVISORY_CASE != root-version ]] || version=v0.0.0
    [[ $SMTP_ADVISORY_CASE != root-replacement ]] || replacement=./other-source
    jq -n --arg version "$version" --arg replacement "$replacement" \
      '{Path:"github.com/emersion/go-smtp",Version:$version,Replace:{Path:$replacement}}'
    ;;
  'mod init'|'mod edit'|'mod tidy') ;;
  'mod download') jq '{Sum:.sum,GoModSum:.goModSum}' "$SMTP_ADVISORY_METADATA" ;;
  *) exit 2 ;;
esac
GO
cat >"$fixture/scanner" <<'SCANNER'
#!/usr/bin/env bash
set -euo pipefail
[[ $* == '-json -scan=module' ]] || exit 2
version=$(jq -r .version "$SMTP_ADVISORY_METADATA")
case "$SMTP_ADVISORY_CASE" in
  missing) printf '{}\n'; exit 0 ;;
  wrong) version=v0.0.0 ;;
esac
jq -n --arg version "$version" '{SBOM:{modules:[{path:"github.com/emersion/go-smtp",version:$version}]}}'
if [[ $SMTP_ADVISORY_CASE == finding ]]; then
  printf '{"finding":{"osv":"SYNTHETIC-NOT-A-REAL-ADVISORY"}}\n'
fi
if [[ $SMTP_ADVISORY_CASE == failed ]]; then exit 3; fi
SCANNER
chmod +x "$fixture/bin/go" "$fixture/scanner"
for control in missing wrong finding failed root-version root-replacement root-unreadable; do
  if PATH="$fixture/bin:$PATH" SMTP_ADVISORY_CASE="$control" \
    bash "$root/scripts/check-smtp-upstream-advisories.sh" "$fixture/scanner" >"$fixture/$control.log" 2>&1; then
    echo "FAIL: $control scanner result accepted" >&2
    exit 1
  fi
done
PATH="$fixture/bin:$PATH" SMTP_ADVISORY_CASE=valid \
  bash "$root/scripts/check-smtp-upstream-advisories.sh" "$fixture/scanner" >"$fixture/valid.log" 2>&1
grep -q 'SMTP_UPSTREAM_ADVISORIES=PASS' "$fixture/valid.log"
echo 'PASS: root pin/replacement, exact upstream identity, missing/wrong evidence, findings and scanner failures'
