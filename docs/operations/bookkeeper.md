# Supplier voucher collection

The platform prepares two daily installations of the signed `bookkeeper` chart. Production uses
the business mailbox and the production Numina company. Development uses a separate mailbox
containing synthetic test mail and a separate Numina test company. Both overlays are prepared,
suspended and unselected by the active apps layers. Merging this preparation installs neither
collector, so missing private configuration cannot block apps readiness. Development's independent
health and non-paging deployment lane must exist before it can be selected (ADR 0016).
The collector reads mail, sets its two processing keywords, and uploads invoice attachments to
Numina's inbox. It does not move or delete mail, book entries, reconcile accounts or move money.

## Private configuration

The owner supplies one OpenBao KV entry per environment. Do not copy production credentials or
mail into development. Each entry is readable only through its environment's dedicated Kubernetes
auth role and namespaced SecretStore; the collector pod receives no Kubernetes API token.

| Environment | Namespace | OpenBao KV entry |
| --- | --- | --- |
| Production | `bookkeeper` | `apps/bookkeeper/prod` |
| Development | `bookkeeper-dev` | `apps/bookkeeper/dev` |

Each entry needs exactly these four fields, supplied privately:

| Field | Required value |
| --- | --- |
| `mail-user` | The environment's own JMAP mailbox login. Development must use a distinct synthetic-only mailbox. |
| `mail-password` | The credential for that mailbox, with no administrator authority. |
| `numina-api-key` | The environment's own company's key with `documents:write` only; no booking authority. |
| `since` | The owner's confirmed first day of the books in that company, as `YYYY-MM-DD`. Collection starts at midnight UTC. |

The entries produce separate namespace-local Secrets named `bookkeeper`. The mailbox login and
start date become required chart values; only the mailbox password and upload key are mounted into
the collector. Missing configuration prevents Helm installation. There is no fallback mailbox,
company, start date or credential. The destination API is `api.numina.app`; the mail service is the
platform's public HTTPS JMAP endpoint. Egress allows those two destinations on 443 and their DNS
lookups only. The chart and container are pinned to their verified v1.0.0 publication digests.

## Activation

Keep both overlays unselected until their private configuration entries exist and their mailbox,
company and date have been confirmed. Selecting an overlay with missing required values would hold
its deployment health gate. A rendered manifest does not prove those private facts.

1. Complete and accept the business mailbox cutover and historical-mail import. Confirm that the
   chosen production start date covers the intended books without importing unrelated history.
2. Confirm separate development mailbox and Numina test-company credentials. Put only synthetic
   invoice mail in that mailbox; production mail and credentials remain isolated.
3. After configuration is complete, select each overlay through a reviewed Git change, retaining
   `suspend: true`. Production selection must add its HelmRelease and OCIRepository alert coverage
   and approve the native validator's measured authorization-ledger delta. Development selection
   must use its independent non-paging lane. Read back each Helm release and CronJob, verifying the
   exact image digest and suspension state.
4. Activate development through a reviewed Git change. Observe a successful scheduled Job, inspect
   its counts-only log, and confirm that the synthetic voucher appears in the test company's inbox.
   Repeat collection and confirm that the same mail is skipped without a duplicate voucher.
5. Activate production in a separate reviewed Git change after the test and mailbox acceptance.
   Verify the daily Job and the intended Numina inbox. Keep issues #6158 and #6160 open until their
   actual running and voucher-delivery outcomes have been observed.

Collection runs at 06:15 in production and 06:30 in development, in `Europe/Copenhagen`. The chart
forbids overlapping runs, caps a run at 30 minutes, and logs counts only. Check Job status and the
counts-only log after activation; do not copy mail subjects, file names, supplier details, amounts
or credential values into repository artifacts.
