# replace-openbao-pod

Makes the two production changes of the single-server OpenBao replacement procedure (#4970). The
protected [`Replace OpenBao Server`](../../.github/workflows/replace-openbao-server.yaml) workflow
runs it only after the [health judge](../judge-openbao-health/README.md) and the
[quorum judge](../judge-openbao-quorum/README.md) returned `PROVEN` for the named server. The
procedure is described in the [runbook](../../docs/operations/openbao-server-replacement.md).

```sh
replace-openbao-pod delete --server openbao-2 --expect-uid <uid-from-the-judged-capture> --timeout 10m
replace-openbao-pod deliver --timeout 5m
```

| Step | Exit | Line | Meaning |
| --- | --- | --- | --- |
| `delete` | 0 | `OPENBAO_POD_REPLACEMENT=REPLACED` | The pod was deleted and a successor with a different UID exists. |
| `delete` | 1 | `OPENBAO_POD_REPLACEMENT=REFUSED reason=pod-changed-since-capture` | The pod no longer has the judged UID, so nothing was deleted. |
| `delete` | 2 | `OPENBAO_POD_REPLACEMENT=UNVERIFIED stage=<stage>` | `pod-identity`, `delete` or `successor` failed or ran out of time. |
| `deliver` | 0 | `OPENBAO_SECRET_DELIVERY=CONFIRMED` | The canary ExternalSecret synced again after the request, successfully. |
| `deliver` | 1 | `OPENBAO_SECRET_DELIVERY=FAILED` | It synced again after the request, and the sync failed. |
| `deliver` | 2 | `OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=<stage>` | `canary-read`, `sync-request` or `sync` failed or ran out of time. |

Invalid arguments also exit 2 and change nothing, including any flag given twice.

- **delete** sends the delete with a UID precondition, so the API server refuses it when the pod was
  replaced after the capture. Whether the successor is healthy is the judges' call, not this
  command's.
- **deliver** changes the `force-sync` annotation on `openbao/vault-snapshot-r2`, which External
  Secrets reads as a request to sync now. It counts only a sync with a new synced version and a
  refresh time no earlier than the request.

The only kubectl verbs it uses are `get`, one `delete` and one `annotate`; the regressions pin them.
It prints one status line and never a pod UID, secret or response body.

Run the regressions with `go test ./scripts/replace-openbao-pod`. They stand in for kubectl, and they
run the workflow's shared judgement, [`judge-openbao-replacement.lib.sh`](../judge-openbao-replacement.lib.sh),
against stand-ins for the collector and both judges.
