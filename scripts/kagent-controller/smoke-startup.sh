#!/bin/sh
set -eu

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

printf '%s\n' 'postgresql://127.0.0.1:1/kagent?sslmode=disable&connect_timeout=1' >"$workdir/database-url"
cat >"$workdir/controller.yaml" <<'YAML'
database:
  skipMigrations: true
  readWrite:
    urlFile: database-url
    role: kagent_readwrite
harnessImages:
  release: "1.0.0"
YAML

/app --config "$workdir/controller.yaml" --check-config

# The isolated image must pass the built-in harness catalog check and reach
# PostgreSQL. The fixture deliberately has no database or network access.
if /app --config "$workdir/controller.yaml" >"$workdir/startup.log" 2>&1; then
  printf '%s\n' 'Controller unexpectedly started without a database.' >&2
  exit 1
fi
cat "$workdir/startup.log"
grep -Fq 'verify database migrations:' "$workdir/startup.log"
grep -Fq 'connection refused' "$workdir/startup.log"
