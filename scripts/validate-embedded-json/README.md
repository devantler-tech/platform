# Embedded JSON validation

Schema validation sees ConfigMap data as strings. This offline command also
checks JSON syntax, so a malformed embedded document fails before deployment.

From the repository root:

```sh
bash scripts/validate-embedded-json.sh
go test ./scripts/validate-embedded-json
```

The Bash entrypoint locates its checkout, so its absolute path works from another
directory. The Go command accepts `-root <repository>` for isolated fixtures.
No cluster, credentials or network access is needed.

The scanner checks `k8s/**/*.yaml` and `k8s/**/*.yml` ConfigMap documents. It
selects direct `data` keys ending in `.json` or listed in `registeredKeys` in
`main.go`. Add new JSON keys without that suffix to the registry. Literal blocks
(`|`) and single-line values are checked; folded blocks (`>`) are rejected because
they change JSON whitespace. Encrypted `.enc.yaml` files and values containing
`ENC[` are skipped. Manifest schema validation remains responsible for YAML
syntax; this retains the existing plain-key source scanner and does not decode
YAML quoting or inspect arbitrary ConfigMap values.

For compatibility, complete unquoted JSON values `NaN`, `Infinity` and
`-Infinity` remain accepted. Their occurrence inside strings is unchanged;
malformed identifiers are rejected. Errors are sorted by path and include the
ConfigMap key line, key name and JSON line/column/character position. The native
Go JSON parser supplies the error wording.
An incomplete scan or a failed result write exits unsuccessfully; neither
reports successful validation.
