# Platform Longhorn runtime

These components select the signed Platform manager and chart needed for an
iSCSI-free V2 installation. No production reconciliation root references them.
The chart and manager are pinned to the verified publication of
`1.13.0-platform.8`. Flux accepts only the dedicated main-branch publisher.
They use public artifacts and require no additional registry credential.

Choose exactly one component in the production controllers composition:

| Component | Capability | Engine settings | Chart-owned StorageClass |
| --- | --- | --- | --- |
| `longhorn-platform-runtime/` | V1-capable | Preserved | Preserved |
| `longhorn-v2-engine-settings/` | V1-capable | V1 disabled, V2 enabled | Disabled |
| `longhorn-v2-only/` | V2-only | V1 disabled, V2 enabled | Disabled |

Each change is a separate reviewed rollout with native readback before the next.
The component reference is the deployment switch; these declarations alone
establish no adoption, recovery, capacity or host-removal acceptance.

## Adopt the packages

Reference this directory first, retaining iSCSI and the current engine and
provisioning settings. Verify the immutable chart's SourceVerified condition,
the manager image and rollout, serving volume identities and health, and the
existing authenticated read-only UI. The manager rolls one Pod at a time.

This does not change the instance-manager image. The temporary V1 retirement
runtime has its own lifecycle under #6073 and must be removed and verified
before V2 activation unless another compatible route is separately qualified.

## Prepare the installed V2 state

Before referencing `longhorn-v2-engine-settings/`, complete the V2 storage and independent
recovery proofs. Account for active and retained Longhorn and provider-backed
data, historical snapshots and original copies. Verify all required data on V2,
native application health and redundancy, and the absence of V1 volumes,
engines, replicas and instance-manager runtime.

Move every declarative workload owner and the provisioning default to the
separately qualified external V2 StorageClass. Preserve legacy classes until
their claims and ownership have been accounted for. Disabling chart ownership
avoids changing the generated StorageClass definition, which would otherwise
cause the manager to delete and recreate that class.

After this rollout, verify the installed V1=false/V2=true settings and external
StorageClass ownership. Keep iSCSI installed during this step.

## Select V2-only mode

Only then reference `longhorn-v2-only/`. The pre-upgrade hook validates the previously
installed settings and runtime before Helm applies new values, so combining the
preparation and final switch is not a substitute for the intermediate readback.
The manager also refuses incompatible live storage at startup.

Verify the V2-only manager and global commands, native V2 attachment and recovery,
and application behavior. Remove iSCSI through the reviewed node configuration
and controlled rollout only after those checks pass. Verify every current node
and future machine definition.

The workaround and its removal are tracked in
[platform#5781](https://github.com/devantler-tech/platform/issues/5781).
Adopt an upstream replacement through ordinary review and native qualification
when one supports the required behavior; publication alone is not that proof.
