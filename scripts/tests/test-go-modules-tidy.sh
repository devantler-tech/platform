#!/usr/bin/env bash
# Exercise the actual read-only guard with real Go and a tiny offline module proxy.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'chmod -R u+w "$work"; rm -rf -- "$work"' EXIT
fixture="$work/repo with spaces"
mkdir -p "$fixture/scripts" "$work/proxy/example.com/tiny/@v" "$work/zip/example.com/tiny@v1.0.0"
export GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local
export GOPROXY="file://$work/proxy" GONOPROXY=none GOSUMDB=off GOMODCACHE="$work/modules"
printf 'module example.com/tiny\n\ngo 1.24.0\n' >"$work/zip/example.com/tiny@v1.0.0/go.mod"
printf 'package tiny\nconst Value = 1\n' >"$work/zip/example.com/tiny@v1.0.0/tiny.go"
cp "$work/zip/example.com/tiny@v1.0.0/go.mod" "$work/proxy/example.com/tiny/@v/v1.0.0.mod"
printf '{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}\n' >"$work/proxy/example.com/tiny/@v/v1.0.0.info"
printf 'v1.0.0\n' >"$work/proxy/example.com/tiny/@v/list"
(cd "$work/zip" && zip -qrD "$work/proxy/example.com/tiny/@v/v1.0.0.zip" example.com)
git -C "$fixture" init -q
new_module() {
  local dir="$1" name="$2"
  mkdir -p "$fixture/$dir"
  printf 'module example.com/%s\n\ngo 1.24.0\n\nrequire example.com/tiny v1.0.0\n' "$name" >"$fixture/$dir/go.mod"
  printf 'package fixture\nimport "example.com/tiny"\nvar Value = tiny.Value\n' >"$fixture/$dir/fixture.go"
  go -C "$fixture/$dir" mod tidy
  git -C "$fixture" add -- "$dir/go.mod" "$dir/go.sum" "$dir/fixture.go"
}
new_module . root
new_module 'nested module' nested
cp "$fixture/go.mod" "$work/clean.mod"
cp "$fixture/go.sum" "$work/clean.sum"

# The pre-fix test-only job accepts an unused requirement. This is the original
# failure mode, exercised before depending on the new guard.
go -C "$fixture" mod edit -require=example.com/unused@v1.0.0
go -C "$fixture" test ./...
if go -C "$fixture" mod tidy -diff >"$work/original-gap" 2>&1; then
  echo 'FAIL: the unused-requirement control was already tidy' >&2
  exit 1
fi
cp "$root/scripts/check-go-modules-tidy.sh" "$fixture/scripts/"
passed=0
expect() {
  local wanted="$1" diagnostic="$2" status=0 file
  # Save every existing module file, including untracked sum files, to prove
  # that a refusal cannot silently repair the candidate under test.
  find "$fixture" -type f \( -name go.mod -o -name go.sum \) -print0 >"$work/before-list"
  mkdir -p "$work/before"
  while IFS= read -r -d '' file; do
    mkdir -p "$work/before/$(dirname "${file#"$fixture"/}")"
    cp "$file" "$work/before/${file#"$fixture"/}"
  done <"$work/before-list"
  bash "$fixture/scripts/check-go-modules-tidy.sh" >"$work/result" 2>&1 || status=$?
  if { [ "$wanted" = pass ] && [ "$status" -ne 0 ]; } || { [ "$wanted" = fail ] && [ "$status" -eq 0 ]; }; then
    cat "$work/result" >&2
    echo "FAIL: control $passed expected $wanted, got $status" >&2
    exit 1
  fi
  if ! grep -Fq -- "$diagnostic" "$work/result"; then
    cat "$work/result" >&2
    printf 'FAIL: control %s omitted diagnostic: %s\n' "$passed" "$diagnostic" >&2
    exit 1
  fi
  find "$fixture" -type f \( -name go.mod -o -name go.sum \) -print0 >"$work/after-list"
  cmp "$work/before-list" "$work/after-list"
  while IFS= read -r -d '' file; do
    cmp "$file" "$work/before/${file#"$fixture"/}"
  done <"$work/before-list"
  rm -rf -- "$work/before"
  passed=$((passed + 1))
}
expect fail 'diff current/go.mod tidy/go.mod'
cp "$work/clean.mod" "$fixture/go.mod"
expect pass 'CHECKED 2 tidy Go modules'

# A sum-only change and a missing sum must both fail without writing a repair.
printf 'example.com/unused v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n' >>"$fixture/go.sum"
expect fail 'diff current/go.sum tidy/go.sum'
cp "$work/clean.sum" "$fixture/go.sum"
rm "$fixture/go.sum"
expect fail 'diff current/go.sum tidy/go.sum'
test ! -e "$fixture/go.sum"
cp "$work/clean.sum" "$fixture/go.sum"

# A newly indexed nested module is discovered without a hard-coded inventory.
new_module 'another/nested module' third
go -C "$fixture/another/nested module" mod edit -require=example.com/unused@v1.0.0
expect fail 'diff current/go.mod tidy/go.mod'
go -C "$fixture/another/nested module" mod tidy
expect pass 'CHECKED 3 tidy Go modules'

# Ambient workspace/modfile settings must not redirect checking away from the
# tracked module. A malformed module is an operational refusal, never a pass.
cp "$fixture/nested module/go.mod" "$work/nested.mod"
printf 'not a go.mod\n' >"$fixture/nested module/go.mod"
export GOFLAGS="-modfile=$work/clean.mod" GOWORK="$work/nonexistent.work"
expect fail 'module nested module'
export GOFLAGS='' GOWORK=off
cp "$work/nested.mod" "$fixture/nested module/go.mod"

# Go normally searches parent directories if a module manifest disappears.
# That must not turn an indexed nested module into another check of the root.
rm "$fixture/nested module/go.mod"
expect fail 'missing or linked manifest/directory for module nested module'
ln -s "$work/nested.mod" "$fixture/nested module/go.mod"
expect fail 'missing or linked manifest/directory for module nested module'
rm "$fixture/nested module/go.mod"
cp "$work/nested.mod" "$fixture/nested module/go.mod"
mv "$fixture/nested module" "$work/linked-module"
ln -s "$work/linked-module" "$fixture/nested module"
expect fail 'missing or linked manifest/directory for module nested module'
rm "$fixture/nested module"
mv "$work/linked-module" "$fixture/nested module"

# A module declaring a newer Go is checked with exactly that release, and a
# two-part directive names its .0 release. Go runs a matching binary from PATH
# before it would download one, so a stub proves the selection offline; the
# offline proxy refuses any download.
mkdir -p "$work/toolchain" "$fixture/newer module"
printf 'module example.com/newer\n\ngo 1.99\n' >"$fixture/newer module/go.mod"
git -C "$fixture" add -- "newer module/go.mod"
cat >"$work/toolchain/go1.99.0" <<'SH'
#!/usr/bin/env bash
printf 'stub go1.99.0 ran: %s\n' "$*"
SH
chmod +x "$work/toolchain/go1.99.0"
PATH="$work/toolchain:$PATH" expect pass 'checking it with go1.99.0'
if ! grep -Fq 'stub go1.99.0 ran: mod tidy -diff' "$work/result" || [ "$(grep -c 'newer than the installed' "$work/result")" != 1 ]; then
  cat "$work/result" >&2
  echo 'FAIL: only the newer module may run on its declared release' >&2
  exit 1
fi
# Without that release available the check fails rather than passing.
expect fail 'module newer module'
# An unreadable directive is a refusal, never a fallback to the installed Go.
printf 'module example.com/newer\n\ngo latest\n' >"$fixture/newer module/go.mod"
expect fail 'cannot read the go directive of module newer module'
git -C "$fixture" rm -q --cached -f -- "newer module/go.mod"
rm -rf -- "$fixture/newer module"
expect pass 'CHECKED 3 tidy Go modules'

# A producer that emits a plausible prefix and then fails must not run Go.
mkdir "$work/bin"
cat >"$work/bin/git" <<'SH'
#!/usr/bin/env bash
if [ "${DISCOVERY_CONTROL:?}" = partial ]; then
  printf 'go.mod\0'
  exit 7
fi
exit 0
SH
cat >"$work/bin/go" <<'SH'
#!/usr/bin/env bash
touch "${GO_CALLED:?}"
exit 0
SH
chmod +x "$work/bin/git" "$work/bin/go"
export PATH="$work/bin:$PATH" GO_CALLED="$work/go-called" DISCOVERY_CONTROL=partial
expect fail 'cannot discover tracked Go modules'
test ! -e "$GO_CALLED"
export DISCOVERY_CONTROL=empty
expect fail 'no tracked Go modules'
test ! -e "$GO_CALLED"
echo "Go module tidiness: $passed real guard controls passed; test-only gap reproduced."
