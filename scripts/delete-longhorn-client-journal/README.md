# Delete the retired Longhorn client journal

This one-off cleanup is for #6472. The retired Longhorn client-maintenance tool
kept a journal ConfigMap, `flux-system/longhorn-client-maintenance`, that Flux
never managed, so removing the tool's code leaves the object in production.
Run the **Delete Longhorn Client Journal** workflow from reviewed main, first
attempt, with confirmation `delete-longhorn-client-journal`. It shares
production deployment serialization through `prod-operation.yaml`.

The program deletes nothing unless all of these hold:

- the dispatched revision no longer contains `scripts/maintain-longhorn-clients`,
  so no deploy or heal can run code that still reads the journal;
- the live object is that ConfigMap, with no labels, owner references or
  finalizers, holding only its journal record;
- the record reports `Phase=complete` and its holder matches the object's
  ownership annotation.

The delete carries UID and resourceVersion preconditions from the object it
read, so the API server refuses a replaced or changed journal. A confirming read
must then find the object gone.

| Verdict | Exit | Meaning |
| --- | --- | --- |
| `DELETED` | 0 | The journal was deleted and a later read confirms it is gone. |
| `ABSENT` | 0 | There was no journal to delete. |
| `HOLD` | 1 | A safety condition failed; nothing was deleted, or the delete was refused. |
| `UNKNOWN` | 3 | Evidence was unreadable; nothing is claimed. |

Read the encrypted result with `go run ./scripts/read-prod-operation-output <run-id>`.
Remove this program and its workflow once a run reports `DELETED` or `ABSENT`.
