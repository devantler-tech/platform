# replace-openbao-pod

Makes the two production changes of the single-server OpenBao replacement procedure (#4970). The
protected [`Replace OpenBao Server`](../../.github/workflows/replace-openbao-server.yaml) workflow
runs it only after the [health judge](../judge-openbao-health/README.md) and the
[quorum judge](../judge-openbao-quorum/README.md) returned `PROVEN` for the named server. The
procedure is described in the [runbook](../../docs/operations/openbao-server-replacement.md).

```sh
replace-openbao-pod applied
replace-openbao-pod delete --server openbao-2 --expect-uid <uid-from-the-judged-capture> \
  --expect-chart 0.30.7 --expect-release 2.7.2 --expect-applied sha256:<proven-digest> --timeout 10m
replace-openbao-pod deliver --timeout 5m
```

| Step | Exit | Line | Meaning |
| --- | --- | --- | --- |
| `applied` | 0 | `OPENBAO_PLATFORM_REVISION=APPLIED digest=<digest>` | The `flux-system/infrastructure-controllers` layer is Ready at its current generation on that manifests digest. |
| `applied` | 1 | `OPENBAO_PLATFORM_REVISION=REFUSED reason=layer-not-ready` | The layer is reconciling or failing, so either revision may be applied. |
| `applied` | 2 | `OPENBAO_PLATFORM_REVISION=UNVERIFIED stage=layer-read` | The layer could not be read, or has no applied digest. |
| `delete` | 0 | `OPENBAO_POD_REPLACEMENT=REPLACED` | The pod was deleted and a successor with a different UID exists. |
| `delete` | 1 | `OPENBAO_POD_REPLACEMENT=REFUSED reason=<reason>` | Nothing was deleted: `pod-changed-since-capture`, `platform-revision-changed`, `template-not-on-reviewed-release`, `storage-not-mounted` or `storage-not-bound`. |
| `delete` | 2 | `OPENBAO_POD_REPLACEMENT=UNVERIFIED stage=<stage>` | `pod-identity`, `platform-revision`, `deployment-binding`, `storage`, `delete`, `successor` or `storage-retained` failed or ran out of time. |
| `deliver` | 0 | `OPENBAO_SECRET_DELIVERY=CONFIRMED` | The canary ExternalSecret synced successfully after the request. |
| `deliver` | 2 | `OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=<stage>` | `canary-read`, `sync-request` or `sync` failed or ran out of time. |

Invalid arguments also exit 2 and change nothing, including any flag given twice.

- **applied** reports the digest the workflow then proves against its own commit (#6219).
- **delete** first requires the layer to still have the `--expect-applied` digest applied, Ready at
  its current generation, then requires the `openbao` HelmRelease to be Ready at its current generation on the
  reviewed chart, and the StatefulSet's live template to run the reviewed release. It then requires
  the `openbao` container to mount exactly `data-<server>` and `audit-<server>`, both Bound. It sends the delete
  with a UID precondition, so the API server refuses it when the pod was replaced after the capture,
  and then waits for a successor that mounts the same claims with the same UIDs. Whether the successor is healthy is the judges' call, not this
  command's.
- **deliver** changes the `force-sync` annotation on `openbao/vault-snapshot-r2`, which External
  Secrets reads as a request to sync now. External Secrets records a new synced
  version only after a successful sync with the changed annotation, so only that counts; a failed
  read records none and the wait runs out.

The only kubectl verbs it uses are `get` (pods, claims, the HelmRelease, the StatefulSet and the canary), one `delete`
and one `annotate`; the regressions pin them.
It prints one status line and never a pod UID, secret or response body.

Run the regressions with `go test ./scripts/replace-openbao-pod`. They stand in for kubectl, and they
run the workflow's shared judgement, [`judge-openbao-replacement.lib.sh`](../judge-openbao-replacement.lib.sh),
against stand-ins for the collector and both judges.
