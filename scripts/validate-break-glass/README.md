# Named recovery field validation

This local tool reads a KV v2 JSON export from standard input. The export's
`data.data` mapping must contain exactly two string values, `kubeconfig` and
`talosconfig`, holding complete YAML files with their original line breaks.
Serialized documents used as field names, duplicate JSON keys, empty values,
unsupported configuration shapes and trailing documents are rejected.

The tool prints generic verdicts only. It never prints keys, values, configuration
contents, parser excerpts or filesystem error details. Input is bounded to 1 MiB
and JSON nesting to 64 levels. Run it without shell tracing:

```bash
go build -mod=readonly -o /private/tmp/validate-recovery-fields ./scripts/validate-break-glass
/private/tmp/validate-recovery-fields < /private/path/recovery-export.json
```

Use an existing private directory for optional extraction:

```bash
/private/tmp/validate-recovery-fields \
  --field kubeconfig --output /private/path/new-recovery.yaml \
  < /private/path/recovery-export.json
```

The destination directory must have no group or other permissions. Extraction
validates both fields, creates a new mode-`0600` file exclusively, and preserves
the selected value's exact bytes. Existing files and symlinks are refused. A
failed write removes the partial output; cleanup failure is reported generically.
Remove temporary exports, extracted files and the binary after use.

Exit codes are `0` for valid fields or completed extraction, `1` for invalid
structure, and `2` for usage, incomplete reads, failed output or filesystem
failure. Use a built binary to distinguish nonzero codes; `go run` wraps them.

This checks local structure only. It does not retrieve or repair stored values,
verify certificates or authorization, contact an API, or establish production
recovery coverage. Follow the approved private operator procedure for those steps.
The stored-value repair and runtime acceptance remain on #4188.

```bash
go test -race ./scripts/validate-break-glass
```
