# FeatureFlag definitions

The guard uses Kubernetes-compatible YAML 1.1 decoding. Quote variant names such
as `"on"` and `"off"`; an unquoted boolean default is rejected. Duplicate keys
are rejected before validation. Symlinks to YAML, directories or unresolved
targets fail coverage; ordinary non-manifest file symlinks are ignored.

```bash
go run ./scripts/validate-feature-flags [YAML-ROOT]
go test ./scripts/validate-feature-flags
```

CI runs the guard on PRs and merge groups without a path filter. It inspects all
YAML documents and list members under `k8s`, validates `spec.flagSpec`, and
reports how many FeatureFlag resources were examined. No flags is an explicit
empty-coverage result. Unreadable inputs and invalid definitions fail.

The schemas are unmodified copies from the official
[`open-feature/flagd-schemas` release json/json-schema-v0.2.15](https://github.com/open-feature/flagd-schemas/tree/1daf5ff56b48d582187d59e35d48c6e191c23839/json),
commit `1daf5ff56b48d582187d59e35d48c6e191c23839`:

| File | SHA-256 |
| --- | --- |
| flags.json | a9b065cc3e140d10a5e139a3f2bbd2f24d4fe8a728ce824a5f2a1231ed60680b |
| targeting.json | 4194aeab5611f80ce8650a10bed5b18483754587bfb2d7ef093e75a0609a2e75 |

Both are embedded, and the compiler refuses every unregistered schema URI.
Validation makes no network request. A schema update replaces both upstream
files and refreshes these checksums in a reviewed change.

The flagd schema permits a null code-defined default for general flagd inputs;
the OpenFeature operator's `v1beta1` CR requires a string `defaultVariant`.
The guard also checks that it names a defined variant. It does not attempt to
prove targeting outcomes, SDK integration, or live flag consumption.
