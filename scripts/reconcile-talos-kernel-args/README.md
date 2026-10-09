# Talos kernel-argument reconciliation

The offline Talos gate renders and patches both roles before validating either.
This helper then applies KSail's post-installer kernel-argument fold: when
normalized Image Factory extensions and either role's normalized arguments are
nonempty, remove `machine.install.extraKernelArgs` and set
`machine.install.grubUseUKICmdline: true` on both roles. Without extensions or
arguments, or with a nonblank explicit `spec.cluster.talos.schematicId`, both
files stay byte-for-byte unchanged. An explicit ID prevents KSail from passing
extensions to config generation; a whitespace-only ID does not. Auxiliary documents keep
their values and order; this does not migrate dedicated Talos install documents.

## Which KSail releases the mirror is valid for

The mirror is valid for a KSail release when the parts of the KSail source the
fold depends on are byte-identical to a set that was audited against it. Those
fold inputs are:

| Input | Why it matters |
| --- | --- |
| `pkg/fsutil/configmanager/` | Holds the fold itself (`talos/configs.go`) and its distribution caller (`ksail/distribution.go`). |
| `pkg/fsutil/generator/talos/` | Generates the Talos patches KSail injects into the configuration the fold then reads. |
| `pkg/apis/` | Defines the cluster configuration the caller reads the extensions and schematic ID from. |
| `charts/` | Carries the generated `Cluster` schema published from those types; earlier audits compared it, so it stays part of the set. |
| `go.mod`, `go.sum` | Pin the Talos machinery that encodes the machine configuration. |

`--check-pins` reads all explicit KSail pins in CI, CD and the production
deployment action. They must be explicit release versions and must all agree.
It then fetches that release's tag from the public KSail repository, reads the
git object ID of each input, and passes only when all six equal one set
recorded in [`audited_inputs.go`](audited_inputs.go). A git tree ID covers every
file below it, so equal IDs mean equal content.

A KSail bump that leaves the inputs unchanged therefore passes with no edit
here, whatever the version is called. This includes releases between two
audited ones: 7.194.6 has the inputs audited at 7.194.7, so it now passes.

The check fails, and names what it could not accept, when:

- any input differs from every audited set (the message lists the changed
  inputs against the nearest set);
- the tag cannot be fetched, an input is absent, or an input is not the kind of
  object expected. A failed or partial read never passes;
- the pins are missing, malformed, not explicit versions, or disagree.

The check runs in the unconditional changes job and needs network access to
`github.com`; each fetch has a
one-minute limit and a failed one is retried twice before the check fails. The fold command itself (three arguments) stays offline: it
checks that the pins agree and relies on `--check-pins` for the source audit.

### When the inputs change: what an audit must cover

A failing check is a request for a source audit, not for a new entry copied from
the error message. Before adding a set to `audited_inputs.go`:

1. Diff each changed input between the last audited release and the new one
   (`git diff v<old> v<new> -- <input>` in a KSail checkout).
2. In `pkg/fsutil/configmanager/talos/configs.go`, confirm `applySchematic`,
   `schematicKernelArgs` and `reconcileFoldedKernelArgs` still fold as
   described at the top of this file: same trigger (normalized extensions and
   arguments both nonempty), same result on both roles. If they do not, change
   `fold` in `main.go` and its tests in the same pull request.
3. In `pkg/fsutil/configmanager/ksail/distribution.go`, confirm an explicit
   nonblank schematic ID still keeps extensions away from config generation.
4. For a `pkg/fsutil/generator/talos/` change, confirm no generated patch sets
   `machine.install.extraKernelArgs` or `grubUseUKICmdline`, which would change
   when the fold triggers, and review what the changed patch does to this cluster.
5. For a `pkg/apis/` or `charts/` change, confirm the `spec.cluster.talos`
   fields this helper reads (`extensions`, `schematicId`) keep their names,
   types and defaults.
6. For a `go.mod` or `go.sum` change, confirm whether the Talos machinery
   version moved, and if so that it still encodes the install section the same
   way (run `scripts/tests/test-talos-render-kernel-args.sh`).
7. Record the new set with the release it was audited at, and add a paragraph
   to the history below saying what changed and why the mirror still holds.

Read the object IDs for the new set from the tag itself:

```bash
git fetch --depth=1 --filter=blob:none https://github.com/devantler-tech/ksail.git refs/tags/v<version>
git ls-tree FETCH_HEAD -- pkg/fsutil/configmanager pkg/fsutil/generator/talos pkg/apis charts go.mod go.sum
```

## Audit history

The first audited implementations are KSail **7.193.6**, commit
`a0622ef7d0f3072823832248a4f79612ed3a0e69`, **7.193.8**, commit
`00e671710f10bb251d54ecd71fac3d36634e66be`, **7.194.0**, commit
`bbe44951bfd0adcc570080c48007866460c709ca`, **7.194.3**, commit
`6c2d2f4b14594521e5001dec9a2796e7902ad610`, **7.194.4**, commit
`37dd45d12e4f445bf8874ee6e59671aa1eefd330`, and **7.194.5**, commit
`d03577f1e0e7465979c97348e5ebb34baf30f4fb`. Their Talos configuration and schematic
code, config manager and KSail distribution caller are byte-identical. The whole
`pkg/fsutil/configmanager/` tree is unchanged between 7.193.8 and 7.194.3. The
7.194.0 and 7.194.3 releases share tree
`d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6`, with every file unchanged.
The 7.194.4 release shares that same configuration-manager tree; its cluster API,
Go dependency inputs and distribution caller are also unchanged from 7.194.3.
Its autoscaler-node shaping repair is in the provisioner and does not alter this
post-installer fold. The 7.194.5 release retains the same configuration-manager,
cluster API and distribution caller trees/blobs. Its dependency versions and
checksums are unchanged; the only module edit marks the existing x/net dependency
as direct. Its dry-run transport repair does not change the mirrored fold.

**7.194.7**, commit `2af05101dd0328cbd9209ed1e435c4bbaebb41b8`, is audited as the
range 7.194.5 → 7.194.7 and is the first audited release whose configuration-manager
tree differs (`439ee0a33cf6ca597f5773bcb6db316900cb8149`). The only changed file in
that tree is `talos/configs.go`, where 7.194.6 makes `applySchematic` return the
computed schematic alongside its ID so KSail can register it with Image Factory before
an upgrade or snapshot uses it. `schematicKernelArgs`, `reconcileFoldedKernelArgs`,
`resolveInstallerVersion` and `applyInstallerImage` are byte-identical to 7.194.5, as
are the distribution caller, the cluster API tree, `go.mod` and `go.sum`; 7.194.7
itself changes only Hetzner bootstrap code. The intermediate 7.194.6 was never
deployed; it now passes because its inputs equal the set audited at 7.194.7.

**7.194.8**, commit `82a359d423a2c1f13a13560b6af0aed9031354c8`, is audited as the
range 7.194.7 → 7.194.8. The configuration-manager tree
(`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the cluster API tree, `go.mod` and
`go.sum` are byte-identical to 7.194.7, so the mirrored fold is unchanged. The only
shipped source changes are in the Hetzner autoscaler provisioner, which now drops the
Longhorn default-disk label from autoscaled workers after the pool labels are applied;
the rest of the release is test and CI plumbing.

**7.194.9**, commit `40e3015e171eb037b31238c021d4b676f1f05858`, is audited as the
range 7.194.8 → 7.194.9. The configuration-manager tree
(`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the cluster API tree, `go.mod` and
`go.sum` are byte-identical to 7.194.8, so the mirrored fold is unchanged. The
shipped source changes are outside the fold: the vcluster provisioner and the retry
helper it shares with KWOK drop an unused recovery path, the local-path-storage
installer takes an injectable transport, the generated chat documentation is
refreshed, and the Talos ingress-firewall generator adds the default pod CIDR
(`10.244.0.0/16`) to the kubelet rule of both the control-plane and the worker rule
sets. That last change does reach this cluster, because KSail injects those
rules at runtime when its own generated patch files are absent, as they are here. It
widens nothing in practice: `talos/*/allow-internal-nodepod-ingress.yaml` already
admits the pod CIDR to the kubelet port, and the rule count is unchanged. This is the
only audited release that changes `pkg/fsutil/generator/talos/`
(`8959ca398398f7bde09c312c362de7fb80936073` → `25c9c416e06b77937e68e71a213c1b2e9ed61205`).
No patch it generates sets `machine.install.extraKernelArgs` or
`grubUseUKICmdline`, so the fold triggers exactly as before.

**7.194.10**, commit `40c72e342ec90a5ea5cb50b69be34a5c2e897b90`, is audited as the
range 7.194.9 → 7.194.10. The configuration-manager tree
(`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the cluster API tree, `go.mod` and
`go.sum` are byte-identical to 7.194.9, so the mirrored fold is unchanged. The
release ships no source change at all: its single commit touches only three of
KSail's own CI actions, which keep its system-test registry mirror cache complete
and within the cache budget.

**7.195.0**, commit `5d7556de76d1a6343f6e41ea8b9754deac09a99c`, is audited as the
range 7.194.10 → 7.195.0. The configuration-manager tree
(`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the cluster API tree, the chart
tree, `go.mod` and `go.sum` are byte-identical to 7.194.10, so the mirrored fold
is unchanged. Its single commit changes `ksail workload validate`: the command
now evaluates the source's own Kyverno policies by default when the
configuration names Kyverno as the policy engine, as both `ksail.yaml` and
`ksail.prod.yaml` do. So that an explicit opt-out reaches the command, KSail's
generated assistant tool calls also forward a boolean flag set to false instead
of dropping it. That changes what this repository's CI and deploy validation
steps check, not what KSail writes to a cluster.

**7.195.1**, commit `74fab4c4d8b7a8348a0542988a83aeeb06bab5a6`, is audited as the
range 7.195.0 → 7.195.1. The configuration-manager tree
(`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the cluster API tree, the chart
tree, `go.mod` and `go.sum` are byte-identical to 7.195.0, so the mirrored fold
is unchanged. Its two commits reword what the `Unmanaged` marker in
`ksail cluster list` means, and fix the kubeadm bootstrap on Hetzner: IPv4
forwarding is enabled before kubeadm runs, and the bring-up stops waiting once
cloud-init reports a failure. The Hetzner change lives in the bring-up base that
only the K3s and kubeadm Hetzner provisioners use; the Talos provisioner this
cluster runs on does not.

**7.197.1**, commit `75ff5337808c358631206aefb39c809cf788d33c`, is audited as the
range 7.195.1 → 7.197.1, which also covers 7.196.0 and 7.197.0. The
configuration-manager tree (`439ee0a33cf6ca597f5773bcb6db316900cb8149`), the Talos
generator tree, the cluster API tree and the chart tree are byte-identical to
7.195.1, so the mirrored fold is unchanged. 7.196.0 and 7.197.0 still carry
the 7.195.1 set in full; 7.197.1 is the first release audited since 7.194.5 that
changes `go.mod` and `go.sum`. Four modules moved and nothing else:
`golang.org/x/oauth2` 0.36.0 → 0.37.0, the Azure SDK `azcore` 1.23.0 → 1.23.1 and
`azidentity` 1.14.0 → 1.14.1, and the Microsoft authentication library 1.7.2 →
1.8.0. The Talos machinery version did not move. Of the four, the package that
holds the fold reaches only `golang.org/x/oauth2`, through the Kubernetes client
transport, and the two packages it reaches there (`oauth2` and `oauth2/internal`)
have byte-identical sources in both versions; that release changes only Google
default credential lookup and the Go version its own module declares. The shipped source changes are outside the fold: 7.196.0 adds an
experimental, opt-in option to `ksail workload validate` that also checks the
resources an operator generates from the manifests; it is off by default and no
validation step in this repository passes it, so what they check is unchanged;
7.197.0 makes `ksail project env reconcile` available without the experimental
flag; and 7.197.1, besides the four module updates, pins two telemetry modules in KSail's
separate desktop module. Three smaller changes in the range are inert here: the
default Argo CD chart moves from 10.9.2 to 10.9.4, which this Flux-managed cluster
never installs; the throwaway-cluster validation behind `--ephemeral` records what
it applied, which no step here uses; and a standalone tool that checks Hetzner node
user-data in KSail's own tests is added. See
[`applySchematic`, `schematicKernelArgs` and `reconcileFoldedKernelArgs`](https://github.com/devantler-tech/ksail/blob/6c2d2f4b14594521e5001dec9a2796e7902ad610/pkg/fsutil/configmanager/talos/configs.go#L1082)
and the [explicit schematic selection boundary](https://github.com/devantler-tech/ksail/blob/6c2d2f4b14594521e5001dec9a2796e7902ad610/pkg/fsutil/configmanager/ksail/distribution.go#L154).
KSail first computes the schematic and installs its image; this helper mirrors
the subsequent fold, using CI's already-generated install sections. It does not
register a schematic, replace installer images, contact a cluster or change
production configuration. Missing machine/install sections are not fabricated.

**7.202.3** is audited against 7.197.1 for the released same-version boot-image
recovery repair (#4668). The configuration manager adds a provider-specific
Kubernetes network patch, applied only for the nested Kubernetes provider;
the production Hetzner provider returns its existing configuration unchanged.
`applySchematic`, `schematicKernelArgs`, `reconcileFoldedKernelArgs` and the
explicit schematic-selection boundary are unchanged. The Talos generator tree
is byte-identical. The cluster API and chart edits add nested-network defaults
and clarify floating-IP and schema-location behavior; the extensions and
schematic-ID fields retain their names, types and defaults. The module inputs
include unrelated dependency updates, but the Talos machinery and YAML encoding
versions used by the fold do not move. The offline render regression verifies
both roles with the pinned Talos client. This adoption leaves the intended
production schematic and strict node-image readback unchanged.

**7.202.28**, commit `349f4a104cc9fe6d3b8db2e963b6203246ad8f4f`, is audited
against 7.202.3 for the released cluster-update ownership repair. Among the six
governed inputs, only the K3d Dockerfile image pin, the operator chart README,
`go.mod` and `go.sum` change. The complete Talos generator and cluster API trees
are byte-identical. Both the Talos configuration manager and the distribution
caller are byte-identical, preserving extension activation, the argument union
and reconciliation for both roles, and explicit schematic-ID precedence. The
Talos machinery, Image Factory, YAML and protobuf versions and checksums are
unchanged. Chart manifests, extension defaults and schematic-ID defaults are
unchanged. The offline render regression verifies both roles in production and
local configurations with the pinned Talos client. The ownership repair changes
how cluster updates cooperate with existing Helm owners; this adoption preserves
the intended production schematic and strict node-image readback.

```bash
go run ./scripts/reconcile-talos-kernel-args --check-pins
go run ./scripts/reconcile-talos-kernel-args ksail.prod.yaml controlplane.yaml worker.yaml
go test ./scripts/reconcile-talos-kernel-args
bash scripts/tests/test-talos-render-kernel-args.sh
```

The real offline test executes the actual CI render step with the pinned
`talosctl`: both production roles must lose their arguments and use the UKI
command line, while both local roles retain their explicit no-extension values.
An invalid machine-type patch must fail for each role in each environment.
The same actual caller preserves both roles' arguments and UKI values with an
explicit schematic ID, while a whitespace-only ID still permits folding.
`TALOSCTL_BIN` may point at a locally verified client of the pinned version.
No local cluster is started.

**7.202.3**, commit `f9172ab810fdcb94b47351d04d70708f6878e10a`, is audited as
the range 7.197.1 → 7.202.3. The Talos generator is byte-identical. The changed
configuration manager adds pod/service network patches for the nested Kubernetes
provider; that path is a no-op for this platform's Hetzner provider. The fold's
trigger, argument normalization, both-role output, installer-image selection and
explicit schematic-ID boundary are unchanged. The API and chart changes add
network defaults and clarify existing settings; the extensions and schematic-ID
fields retain their names, types and defaults. Dependency updates do not change
Talos machinery or its YAML encoder. The offline render test still exercises both
roles and rejects invalid patches.

This release also contains [KSail #7300](https://github.com/devantler-tech/ksail/pull/7300):
same-version boot-image changes now trigger a node rollout instead of being
silently skipped. Its [final-head provider trial](https://github.com/devantler-tech/ksail/actions/runs/37591901167)
verified a same-version image update, readiness, a second no-change plan and
cleanup on a temporary single-node cluster. That trial does not cover autoscaled
nodes or an interrupted rollout; the release's regression tests cover those paths.
