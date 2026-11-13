# Replacing an OpenBao server

OpenBao runs as three servers in one Raft cluster. Its StatefulSet updates `OnDelete`, so a chart or
release update reaches a server only when its pod is deleted. The protected **Replace OpenBao Server**
workflow does that one server at a time, and only while every server is proven healthy, so the
secrets service stays writable throughout.

Kubernetes Ready is not used as proof. The readiness probe accepts sealed and uninitialized servers on
purpose, so the bootstrap job can reach them.

## What one run does

Every run is dispatched by hand on `main`, waits for the protected `prod` environment's approval and
shares the `prod-deploy` lock with deployments.

1. **Judge.** It captures every server's health and the Raft configuration, then runs the
   [health judge](../../scripts/judge-openbao-health/README.md) and the
   [quorum judge](../../scripts/judge-openbao-quorum/README.md) for the named server. Anything but
   `PROVEN` from both stops the run. `preflight` mode ends here and changes nothing.
2. **Replace** (`replace` mode only). It deletes the named server's pod, but only while that pod still
   has the UID the judged capture saw; the API server enforces that, so a pod that changed in the
   meantime is never deleted. It then waits up to ten minutes for the successor pod.
3. **Recover.** It captures and judges again every 20 seconds, for up to fifteen minutes, until both
   judges return `PROVEN` for a capture of the successor. During an upgrade the replaced server must
   now run the new release.
4. **Deliver.** It asks External Secrets to re-read the `openbao/vault-snapshot-r2` canary from
   OpenBao and waits for a new, successful sync. Nothing reads that secret at startup, so the re-read
   changes no workload.

The log carries only verdict lines: never a token, pod UID, address or response body. The run passes
only when all four steps do.

## Upgrading to a new release

Merge the chart update first. Its deploy changes no running server, and the next preflight returns
`HOLD` because every server still runs the old release. Then replace the servers one run at a time,
standbys first and the active server last, so the cluster elects a leader only once:

| Run | `server` | `previous_version` | `replaced` | `confirm` |
| --- | --- | --- | --- | --- |
| 1 | a standby, e.g. `openbao-2` | the old release, e.g. `2.6.3` | empty | `REPLACE openbao-2` |
| 2 | the other standby, e.g. `openbao-1` | `2.6.3` | `openbao-2` | `REPLACE openbao-1` |
| 3 | the active server, e.g. `openbao-0` | `2.6.3` | `openbao-2,openbao-1` | `REPLACE openbao-0` |

Run `preflight` with the same inputs first when in doubt; it takes the same judgement and changes
nothing. The new release always comes from the reviewed chart, never from an input, and any third
release is a `HOLD`.

To replace a server without an upgrade, for example to move it to a new node, leave
`previous_version` and `replaced` empty.

## When a run fails

| Last line | What happened | What to do |
| --- | --- | --- |
| `OPENBAO_REPLACEMENT_PREFLIGHT=HOLD` or `UNKNOWN` | Nothing was changed. | Read the judges' finding lines, fix the cause, and run `preflight` again. |
| `OPENBAO_REPLACEMENT=NOT_STARTED` | The judgement in `replace` mode failed. Nothing was changed. | As above. |
| `OPENBAO_POD_REPLACEMENT=REFUSED` | The pod changed after the capture. Nothing was deleted. | Run `preflight` again; something else is replacing servers. |
| `OPENBAO_POD_REPLACEMENT=UNVERIFIED` | The pod could not be read or deleted, or no successor appeared. | Check the StatefulSet and its events before anything else. |
| `OPENBAO_REPLACEMENT=NOT_RECOVERED` | The pod was replaced but the judges never returned `PROVEN`. | Use the recovery path below. Replace no other server. |
| `OPENBAO_SECRET_DELIVERY=UNVERIFIED` | The servers are healthy, but External Secrets did not read from OpenBao. | Check the `openbao` ClusterSecretStore and the External Secrets controller. |

## Recovery path

Replacing one server leaves the other two serving, so the secrets service stays available while you
recover. Replace no other server until the procedure returns `PROVEN` again.

- **The new server is sealed.** It unseals itself from the `openbao-unseal` Secret when it starts.
  Check that its `postStart` hook ran and that the Secret still holds the key, as in Scenario 1 of the
  [OpenBao disaster-recovery runbook](../dr/openbao.md).
- **The new release does not start or does not join.** Revert the chart update in a pull request. The
  workflow cannot replace the failed server, because it replaces a server only while every server is
  proven healthy. Once the revert deploys, an operator with production access deletes that one pod by
  hand (`kubectl --context admin@prod --namespace openbao delete pod <server>`); it restarts on the
  release the other two still run and keeps its data volume, so it rejoins with its Raft state. Then
  run `preflight` until it returns `PROVEN`.
- **Quorum is lost or data is damaged.** Follow Scenario 2 of the
  [OpenBao disaster-recovery runbook](../dr/openbao.md), which restores the newest snapshot.

## Verification

`go test ./scripts/capture-openbao-replacement ./scripts/judge-openbao-health ./scripts/judge-openbao-quorum ./scripts/replace-openbao-pod`
runs the offline regressions; the workflow runs them before it reads any credential. They stand in for
the cluster, so they cannot prove that a real pod is replaced and rejoins. The first `replace` run in
production is that proof; record it on #4970.
