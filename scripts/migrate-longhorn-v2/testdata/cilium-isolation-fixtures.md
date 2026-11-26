# Native recovery isolation fixtures

The input is the actual `cnpgRecoveryResources` CNP rendered with synthetic
Umami archive data, run `1234`, and UID
`11111111-1111-4111-8111-111111111111`. No production data is included.

The entries were generated, not hand-authored, using Cilium v1.20.5:

- Verified annotated tag: `a5d08be2876a03696503ef1bb775ae191353870b`
- Source commit: `150121410c147fad575ac5dcbce6de923be69042`
- Go: `go1.27.3 darwin/arm64`
- Dependencies: the checksum-verified Go module for the signed release
- Agent settings: cluster `default`, local-cluster policy normalization enabled,
  non-default-deny policies enabled, L7 proxy enabled

From that verified Cilium checkout, use the generator in this directory:

```sh
rtk proxy env GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  go run -mod=vendor /absolute/path/to/testdata/cilium-fixture-generator.go \
  default true true true \
  < /absolute/path/to/testdata/cnpg-isolation-input-cilium-1.20.5.json \
  > /tmp/cnpg-isolation-entries-cilium-1.20.5.json
```

The parser replaces only the bound recovery namespace, policy UID, and exact
archive FQDN. It requires the actual CNP spec to match the native-reviewed input
and compares complete imported entry multisets, retaining duplicate counts.
Other Cilium releases or configuration settings require a regenerated reviewed
fixture. This proves imported rules only; it does not prove an agent's current
configuration, a Pod's effective endpoint policy, or network reachability.

## Existing namespace baseline

The namespace also receives the platform's generated default-deny and DNS
policies. Their two synthetic inputs, `cnpg-baseline-{deny,dns}-input-cilium-1.20.5.json`,
contain the exact specs and policy names from
[`add-default-deny.yaml` at reviewed main](https://github.com/devantler-tech/platform/blob/08be2b9daf0a5bd3cf518961b58d95b470bc8068/k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml),
with a synthetic namespace and UID. Run each through the same generator and
concatenate its output arrays to reproduce
`cnpg-baseline-entries-cilium-1.20.5.json`. No generated entries are hand-edited.

The published Cilium Go module resolves to the signed source commit and verifies
through Go's checksum database:
`github.com/cilium/cilium@v1.20.5`, module sum
`h1:kqhMiKzzcSm6rScvoNW4E6Ev3qoXR9xsxiBhNGucNn4=`. Go 1.27.3 with
`-mod=readonly` reproduced the prior owned-policy fixture byte for byte before
compiling the baseline inputs. The module and checksum files remained unchanged.

The baseline compiles to two empty-peer entries that grant nothing and one
resolver-only UDP/TCP port-53 allowance. The latter is an existing platform
allowance, including its absence of DNS-layer restrictions. The owned recovery
policy retains its DNS proxy rule for archive FQDN resolution.

The same admission policy generates a Kubernetes `NetworkPolicy` named
`default-deny` with an empty Pod selector and both policy directions.
`cnpg-baseline-knp-entries-cilium-1.20.5.json` is the v1.20.5
`pkg/k8s.ParseNetworkPolicy` output for that policy with synthetic namespace
`longhorn-db-proof-umami-db-1234` and UID
`44444444-4444-4444-8444-444444444444`. Its two entries have explicit null
peers and ports, which grant no traffic just like the empty lists on the
Cilium default-deny entries. Only these paired, empty, enforced directions
normalize in the allowance comparison. A missing field or a wildcard peer
still fails.

The repository-wide check examines all potentially applicable allowances,
including entries without the owned CNP's labels. Apart from identity labels
and the separately validated subject selector, their complete effect must
equal an owned allowance or one of these fixed baseline effects. Unknown
fields, tiers, priorities, peers or ports hold. A conjunctive namespace
constraint may prove a different namespace out of scope; restrictive denies
do not widen access. Exact import of all owned CNP entries is still required
separately. No policy labels or expected effects are inferred from live output.

## Runtime configuration fixture

The runtime parser reads `config --all -o json`, `version -o json` and
`policy get -o json` from the same bound agent. The repository check requires the
exact imported policy at its observed revision. Cilium's config API omits
`PolicyDefaultLocalCluster`. For this policy, the scoped peers acquire the
local-cluster selector when Cilium imports it with local normalization.
Re-running the policy fixture generator with the second argument `false`
omits those selectors and the resulting entries fail the import check.
The runtime config and version test inputs are synthetic. The protected
workflow binds the agent Pod and container identity around both observations.

## Endpoint serialization fixtures

`synthetic-endpoint-get-cilium-1.20.5.json` and
`synthetic-cep-cilium-1.20.5.json` come from the same pinned source's exported
API models, using `cilium-endpoint-fixture-generator.go`. The native endpoint
model's `Validate` method passes. These are synthetic objects, not observed
agent responses or evidence of live enforcement. Reproduce both from the
verified Cilium checkout:

```sh
rtk proxy env GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  go run -mod=vendor /absolute/path/to/testdata/cilium-endpoint-fixture-generator.go
```

The generator writes to `/tmp/longhorn-v2-cilium-endpoint-synthetic-5582`.
The endpoint lookup is an array; its realized policy ID is the security identity,
not the endpoint ID. The CEP's exact Pod owner omits `controller: true`.
Its optional attachment identifier is absent in this fixture; the parser also
requires a match when that identifier is present. The opaque CNI attachment ID
is not a PostgreSQL container ID or a Pod UID. A CEP's compressed `ready` state
does not prove native endpoint readiness, so the parser requires the agent's
realized revision and enforcement state too.

The caller must still establish the owned recovery workload graph, the exact
agent on the workload's Node, agent process continuity, the imported policy,
and unchanged identities around live network probes. These fixtures authorize
no recovery operation.

## Endpoint notification options

`synthetic-endpoint-options-cilium-1.20.5.json` is generated by
`cilium-endpoint-options-fixture-generator.go` using Cilium's exported option
library and `EndpointConfigurationStatus` model. Its `realized.options` values
are native `Enabled` / `Disabled` strings. Reproduce it from the same verified
checkout:

```sh
rtk proxy env GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  go run -mod=vendor /absolute/path/to/testdata/cilium-endpoint-options-fixture-generator.go \
  > /tmp/synthetic-endpoint-options-cilium-1.20.5.json
```

The endpoint binder takes `.status.realized.options` from the same native
endpoint-get response that supplies the CNI attachment, endpoint identity and
policy state. This avoids a separate lookup through a reusable numeric ID.
Both observation passes compare the complete endpoint binding, including these
options. The standalone model fixture exercises the native option encoding;
neither fixture establishes a deployed endpoint's settings.

## Executable release binding

The executable profile also binds the independently published
[`quay.io/cilium/cilium:v1.20.5` OCI index](https://quay.io/v2/cilium/cilium/manifests/v1.20.5),
whose response body SHA-256 and `Docker-Content-Digest` agree:

- index: `sha256:aa72499f075ad729947ea91b026218aceba24f1a8e4534bf36b8368c7c561526`
- Linux amd64 child: `sha256:a54ced05354670afb7fecd075e91130dff5cca16518da7ff3d1ef093c5c2d0ae`
- Linux arm64 child: `sha256:dc2a1862e29de053e588980e121acb8b2e534206e9b3047ef5386f8c1eb13c29`

Require the exact index in both the DaemonSet and Pod declarations, then the
index or matching Linux architecture child in the native container image ID.
An arbitrary digest with matching version output is insufficient. These pins
do not establish current deployment, process continuity, or policy enforcement.

## Policy enforcement before a restore Pod starts

For Cilium 1.20.5, ordinary CNI creation requests synchronous endpoint
regeneration. Initial policy-map population precedes BPF installation, and a
realized revision is published only after successful regeneration. With
`RestoreState=true`, endpoint creation also waits for startup cache sync,
including CNP repository insertion. The runtime reader requires the actual
top-level JSON Boolean `RestoreState` from `config --all -o json`; the CLI flag
is `--restore`. Neither its default nor a ConfigMap establishes that observation.

A Pod can initially use `reserved:init`, whose policy defaults to deny even in
default enforcement mode. Existing explicit allowances can change that result.
The full native repository therefore must be checked for potential init egress
allows, including empty clusterwide selectors. The guard requires every
non-node egress Allow subject to have positive namespace-name equality, `In`,
or `Exists` constraints; init does not yet have that namespace label. Negative,
missing, unknown, or malformed selectors hold. This intentionally also refuses
harmless init allowances rather than interpreting peers, ports, or policy tiers.

Before creating a recovery Cluster, create a fresh empty owned namespace and
its all-Pod isolation policy, select and pin the recovery Node, and obtain exact
repository readback from that bound agent. Policy import and the init guard
must use the same repository/revision. The import log only acknowledges a
queued update and is not proof of insertion. An empty subject-selector cache
also cannot prove the absence of init allowances.

After creation, require every actual recovery/Job endpoint to have the correct
owner and selected identity and a realized revision at least as new as the
import. Preserve agent/Node/process continuity around the observation and fixed
live probes. These source guarantees do not replace those live checks or
authorize production claim mutation.
