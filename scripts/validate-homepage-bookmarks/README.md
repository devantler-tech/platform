# Homepage bookmark validation

Run `bash scripts/validate-homepage-bookmarks.sh` from any directory. The wrapper
builds the repository's Go command and removes the temporary binary after use.
Optional arguments select a ConfigMap and the directory to inspect for discovered
service groups. The defaults are the Homepage ConfigMap and `k8s/` in this repository.

The validator checks icons, HTTPS links, duplicate bookmark names, service-group
collisions and layout coverage. Service groups come independently from embedded
`services.yaml` and discovery annotations, so the layout cannot validate itself.
Empty coverage fails. Malformed or ambiguous YAML and failed file observations
cannot produce a clean verdict. A symlink to a file is read; a missing target or
directory target is an incomplete observation.

Exit codes are `0` for valid bookmarks, `1` for violations and `2` when the inputs
cannot be checked. The Go command takes both input paths explicitly:

```sh
go run ./scripts/validate-homepage-bookmarks \
  k8s/bases/apps/homepage/config-map.yaml k8s
go test ./scripts/validate-homepage-bookmarks ./scripts/tests/homepage-bookmarks
```

Use the compatibility wrapper or a built binary when the caller needs to distinguish
exit `1` from `2`; `go run` wraps a program's nonzero exit status.
