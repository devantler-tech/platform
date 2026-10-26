#!/bin/sh
# Staged ONLY by the confirmed retirement dispatch, beside the shared credential.
# The normal coverage proof never stages this file. It consumes an exact object
# set that the runner has proved covered and unchanged; no recursive operation,
# bucket removal, retention bypass, version purge or caller-selected prefix exists.
set -eu
fail() {
  printf 'wedding-backup-removal: %s\n' "$1" >&2
  exit 1
}
[ "$#" -eq 0 ] || fail 'no arguments are accepted'
if [ "${ROLE:-}" != shared ] || [ "${BUCKET:-}" != platform-backups ] ||
  [ "${PREFIX:-}" != cnpg/wedding-db ] ||
  [ "${REMOVAL_CONFIRM:-}" != remove-wedding-shared-backups-4481 ]; then
  fail 'the exact shared location and retirement confirmation are required'
fi

work="${WORK_DIR:-/work}"
[ -s "${work}/listing" ] || fail 'the current coverage listing is missing'
# Consume and validate the complete bounded plan before the first write.
head -n 10001 >"${work}/removal-keys"
count="$(wc -l <"${work}/removal-keys" | tr -d ' ')"
if [ "${count}" -lt 1 ] || [ "${count}" -gt 10000 ]; then
  fail 'the object count is outside its reviewed bounds'
fi
: >"${work}/removal-targets"
while IFS= read -r key; do
  printf '%s\n' "${key}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]*/(base|wals)/[A-Za-z0-9._/-]+$' || fail 'an object key is outside the catalogue layout'
  case "/${key}/" in
    *'//'*|*'/../'*|*'/./'*) fail 'an object key contains an unsafe path component' ;;
  esac
  grep -qF "\"key\":\"${key}\"" "${work}/listing" || fail 'an object key was not in the current coverage listing'
  printf 'store/%s/%s/%s\n' "${BUCKET}" "${PREFIX}" "${key}" >>"${work}/removal-targets"
done <"${work}/removal-keys"
[ "$(sort -u "${work}/removal-targets" | wc -l | tr -d ' ')" = "${count}" ] || fail 'the object set repeats a key'

# Positive controls for every other shared-backup consumer. Keep the complete
# private listings in this pod and require every pre-existing record afterwards;
# new backups may appear concurrently. A retention race fails conservatively.
snapshot() {
  destination="$1" output="$2"
  mc ls --json --recursive "store/${BUCKET}/${destination}/" >"${output}" 2>"${work}/snapshot-error" </dev/null ||
    fail 'an unaffected backup location could not be listed'
  grep -q '"type":"file"' "${output}" || fail 'an unaffected backup location holds no objects'
  while IFS= read -r line; do
    case "${line}" in
      *'"status":"success"'*) ;;
      *) fail 'an unaffected backup listing is incomplete or invalid' ;;
    esac
  done <"${output}"
}
for prefix in cnpg/umami-db cnpg/coroot-db velero/prod; do
  snapshot "${prefix}" "${work}/before-${prefix##*/}"
done

# The credential was configured by the read-only serve command. Keep the native
# client's response and error details inside the run-owned pod: neither can leak
# an endpoint or credential into the public workflow log. The runner independently
# proves the prefix empty afterwards, rather than trusting an rm success message.
if ! timeout 1200 mc rm --json --force --stdin <"${work}/removal-targets" \
  >"${work}/removal-result" 2>"${work}/removal-error"; then
  fail 'object removal failed; the prefix must be checked before any retry'
fi
for prefix in cnpg/umami-db cnpg/coroot-db velero/prod; do
  snapshot "${prefix}" "${work}/after-${prefix##*/}"
  while IFS= read -r line; do
    grep -qxF "${line}" "${work}/after-${prefix##*/}" || fail 'an unaffected backup record changed during removal'
  done <"${work}/before-${prefix##*/}"
done
printf 'wedding-backup-removal: completed the bounded object removal request\n'
