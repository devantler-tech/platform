# V1 retirement runtime

This component selects the reviewed instance-manager publication for the V1
baseline and its guarded scratch cleanup. It is not referenced by production.
Its image declaration includes the release tag and immutable digest required by
the scoped admission policy. Manager controller updates allow one unavailable
DaemonSet Pod at a time.

Enabling the component changes the manager's instance-manager image argument.
The manager sets the read-only default-image Setting through its supported
startup path. Existing busy V1 managers can remain alongside the new generation;
their serving processes must retain their identities and health during adoption.

The protected migration workflow's `inventory-v1-runtime` operation takes no
node or database target and requires that exact confirmation. It reads complete
runtime inventories twice between current-main checks. Its aggregate output and
opaque epoch digest reveal no Pod environment or node addresses. It permits no
host commands, instance-manager RPC, provider operation or Kubernetes mutation.
The existing workflow still performs its protected API-endpoint setup.

The result is observation only: `adoption=UNVERIFIED headroom=UNVERIFIED`. It
neither proves complete runtime behavior nor reserves CPU/RAM. It must not be
used as the adoption or benchmark acceptance receipt.

The image knob also affects V2. Before activation, prove the observed engine and
automatic-upgrade settings, complete serving identities, original volume health,
recovery ownership and CPU/RAM overlap headroom. Keep the worker/pool and provider
capacity gates for their separate operations. A successful rendering or inventory
does not authorize activation, a benchmark, retirement or a worker rebuild.

Activation and removal are separate protected changes tracked by
[platform#6073](https://github.com/devantler-tech/platform/issues/6073). Restore
the normal runtime before V2 activation unless a separately reviewed qualification
explicitly establishes another compatible route. Runtime publication, adoption
and the benchmark cleanup acceptance are separate outcomes.
