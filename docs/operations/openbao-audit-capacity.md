# OpenBao audit capacity recovery

The file audit device appends records and can block requests when its filesystem
fills. Capacity recovery preserves those records; it does not implement archival
or retention. Do not truncate the log, delete claims, replace the StatefulSet or
change its immutable claim template to create headroom.

## Approved scope

The manual **Expand OpenBao Audit Capacity** workflow expands only the three
existing `audit-openbao-0`, `audit-openbao-1` and `audit-openbao-2` claims to
`20Gi`. Database and snapshot claims, audit settings and records remain untouched.
The protected `prod` environment and shared `prod-deploy` concurrency group
serialize the operation with normal deployments. Environment secrets, reviewers,
wait timers and branch policy remain required, but this operation creates no
artifact-deployment record. A future custom deployment-app protection rule is
incompatible with this mode and fails closed; do not disable it. No schedule
initiates expansion. The dispatch defaults to read-only `preflight`.

Capacity cannot subsequently shrink. The workflow reads the project owner's
current EUR gross volume price from Hetzner's pricing API immediately before
each patch. It conservatively budgets the entire 60 GiB target as 66 provider GB
against the approved additional-cost ceiling of €5/month. An unknown currency,
price, response or price above the ceiling stops the operation. The token stays
in memory and neither pricing payloads nor audit data are printed.

## Reviewed execution

1. Merge the reviewed implementation with all required checks passing. Establish
   the successful CI merge-group or manual CD run that deployed the current main
   commit, its successful production deployment record and accepted OCI digest.
2. Use OIDC-only reads to confirm the four root Kustomizations and root source
   are ready at that digest, OpenBao has three healthy instances and each audit
   claim has its original reciprocal volume binding. Keep raw identifiers and
   service telemetry private.
3. First dispatch the workflow on `main` in `preflight` mode with `accepted_deployment_run`,
   `deployed_revision` (`latest@sha256:` plus the exact digest), and confirmation
   `EXPAND OPENBAO AUDIT ONLY TO 20GI`. The checkout must still be current main.
   The first dispatch attempt is the only executable attempt; never re-run it.
   Preflight runs all source, price, binding, mount and service-health checks,
   including two separated snapshots, but creates no receipt and changes no
   claim. Require `OPENBAO_AUDIT_CAPACITY=PREFLIGHT_OK` before a separate fresh
   `expand` dispatch using the same approved scope and current evidence.
4. The protected operation checks initialized, unsealed HA service health using
   a fixed, token-free `bao status` command on each instance. Two separated
   initial samples must preserve workload, pod, storage-class, claim and volume
   identities. An immutable, run-bound consumption receipt is created before
   any capacity patch. Each patch tests the captured claim UID and resource
   version, and changes only its storage request to `20Gi`.
5. Success requires two separated readbacks showing all claim requests, claim
   capacities and backing-volume capacities at `20Gi`, no pending resize
   conditions, unchanged workload/storage identities and healthy unsealed HA
   service. The only success marker is
   `OPENBAO_AUDIT_CAPACITY=EXPANDED claims=3 target=20Gi preservationSamples=2`.

If any patch may have been submitted, failure reports
`OPENBAO_AUDIT_CAPACITY=HOLD_CAPACITY_MAY_HAVE_CHANGED`, followed by a bounded
`stage` code. Read-only failures similarly report their stage; pricing failures
report only a fixed classification, not credentials, prices or raw responses.
Do not replay, shrink,
delete the receipt or infer that the patch was cancelled. CSI work can continue
after the job stops. Inspect the three claims and original backing identities
with OIDC-only reads; prepare a separately reviewed recovery if required.

Finally inspect live disk usage and Coroot warnings more than once. Larger
capacity is verified recovery, not proof that every historical log warning has
aged out. Track archival, restore/readability and retention separately in #4671.

References: [Kubernetes volume expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims),
[Hetzner pricing API](https://docs.hetzner.cloud/reference/cloud#pricing),
and [OpenBao file audit device](https://openbao.org/docs/audit/file/).
