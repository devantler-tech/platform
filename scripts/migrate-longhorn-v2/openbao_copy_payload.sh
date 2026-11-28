#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
umask 077

fail() {
  printf '%s\n' 'OPENBAO_OFFLINE_COPY=UNVERIFIED' >&2
  exit 1
}

owns_mount() {
  local path=$1 access=$2
  [[ -d "$path" && ! -L "$path" ]] || return 1
  awk -v path="$path" -v access="$access" '
    $5 == path {
      matching++
      if (("," $6 ",") ~ ("," access ",")) qualified++
    }
    index($5, path "/") == 1 { nested++ }
    END { exit !(matching == 1 && qualified == 1 && nested == 0) }
  ' /proc/self/mountinfo
}

check_source() {
  local path=$1 invalid present
  # A freshly formatted ext4 root can contain an inaccessible recovery directory.
  # The source and destination each keep their own filesystem recovery directory.
  invalid=$(find "$path" -xdev -mindepth 1 \
    \( -path "$path/lost+found" -prune \) -o \
    \( \( ! -type f ! -type d \) -o ! -uid 100 -o ! -gid 1000 -o \( -type f -links +1 \) \) -printf x -quit) || return 1
  present=$(find "$path" -xdev -mindepth 1 -maxdepth 1 ! -name lost+found -printf x -quit) || return 1
  [[ -z "$invalid" && "$present" == x ]]
}

check_destination() {
  local path=$1 present
  present=$(find "$path" -xdev -mindepth 1 -maxdepth 1 ! -name lost+found -printf x -quit) || return 1
  [[ -z "$present" ]]
}

archive() (
  local root=$1
  shift
  cd "$root" || return 1
  find . -xdev -mindepth 1 -maxdepth 1 ! -name lost+found -print0 |
    sort -z |
    tar --create --file=- --one-file-system --sort=name --null --files-from=- \
      --format=posix --pax-option='exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime' \
      --numeric-owner --acls --xattrs "$@"
)

copy_checked() {
  local source=$1 target=$2 before after
  # Hash logical contents and metadata. Sparse transfer archives can include
  # process-specific names and filesystem-specific hole maps.
  before=$(archive "$source" | sha256sum) || return 1
  archive "$source" --sparse | tar --extract --file=- --directory="$target" \
    --acls --xattrs --same-permissions --no-same-owner || return 1
  sync -f "$target" || return 1
  after=$(archive "$target" | sha256sum) || return 1
  [[ "$before" == "$after" ]]
}

[[ "$(id -u)" == 100 && "$(id -g)" == 1000 ]] || fail
for path in /source-data /source-audit; do
  owns_mount "$path" ro || fail
  check_source "$path" 2>/dev/null || fail
done
for path in /destination-data /destination-audit; do
  owns_mount "$path" rw || fail
  check_destination "$path" 2>/dev/null || fail
done
roots=$(stat -c '%d:%i' /source-data /source-audit /destination-data /destination-audit | sort -u | wc -l) || fail
[[ "$roots" -eq 4 ]] || fail

copy_checked /source-data /destination-data >/dev/null 2>&1 || fail
copy_checked /source-audit /destination-audit >/dev/null 2>&1 || fail
printf '%s\n' 'OPENBAO_OFFLINE_COPY=VERIFIED data=1 audit=1'
