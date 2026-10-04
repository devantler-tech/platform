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

The audited implementations are KSail **7.193.6**, commit
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
deployed and stays refused. See
[`applySchematic`, `schematicKernelArgs` and `reconcileFoldedKernelArgs`](https://github.com/devantler-tech/ksail/blob/6c2d2f4b14594521e5001dec9a2796e7902ad610/pkg/fsutil/configmanager/talos/configs.go#L1082)
and the [explicit schematic selection boundary](https://github.com/devantler-tech/ksail/blob/6c2d2f4b14594521e5001dec9a2796e7902ad610/pkg/fsutil/configmanager/ksail/distribution.go#L154).
KSail first computes the schematic and installs its image; this helper mirrors
the subsequent fold, using CI's already-generated install sections. It does not
register a schematic, replace installer images, contact a cluster or change
production configuration. Missing machine/install sections are not fabricated.

```bash
go run ./scripts/reconcile-talos-kernel-args --check-pins
go run ./scripts/reconcile-talos-kernel-args ksail.prod.yaml controlplane.yaml worker.yaml
go test ./scripts/reconcile-talos-kernel-args
bash scripts/tests/test-talos-render-kernel-args.sh
```

The version check reads all explicit KSail pins in CI, CD and the production
deployment action. All pins must agree on one of the six audited releases. A
missing, malformed, divergent or unaudited pin fails the unconditional changes
job. Another version requires reviewing the owned KSail source and
updating this contract, rather than silently assuming the fold is unchanged.

The real offline test executes the actual CI render step with the pinned
`talosctl`: both production roles must lose their arguments and use the UKI
command line, while both local roles retain their explicit no-extension values.
An invalid machine-type patch must fail for each role in each environment.
The same actual caller preserves both roles' arguments and UKI values with an
explicit schematic ID, while a whitespace-only ID still permits folding.
`TALOSCTL_BIN` may point at a locally verified client of the pinned version.
No local cluster is started.
