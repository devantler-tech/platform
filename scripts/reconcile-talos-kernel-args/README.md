# Talos kernel-argument reconciliation

The offline Talos gate renders and patches both roles before validating either.
This helper then applies KSail's post-installer kernel-argument fold: when
normalized Image Factory extensions and either role's normalized arguments are
nonempty, remove `machine.install.extraKernelArgs` and set
`machine.install.grubUseUKICmdline: true` on both roles. Without extensions or
arguments, both files stay byte-for-byte unchanged. Auxiliary documents keep
their values and order; this does not migrate dedicated Talos install documents.

The audited implementation is KSail **7.193.6**, commit
`a0622ef7d0f3072823832248a4f79612ed3a0e69`, in
[`applySchematic`, `schematicKernelArgs` and `reconcileFoldedKernelArgs`](https://github.com/devantler-tech/ksail/blob/a0622ef7d0f3072823832248a4f79612ed3a0e69/pkg/fsutil/configmanager/talos/configs.go#L1082).
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
deployment action. A missing, malformed or changed pin fails the unconditional
changes job. A version bump requires reviewing the owned KSail source and
updating this contract, rather than silently assuming the fold is unchanged.

The real offline test executes the actual CI render step with the pinned
`talosctl`: both production roles must lose their arguments and use the UKI
command line, while both local roles retain their explicit no-extension values.
An invalid machine-type patch must fail for each role in each environment.
`TALOSCTL_BIN` may point at a locally verified client of the pinned version.
No local cluster is started.
