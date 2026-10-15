# Actual Budget

[Actual Budget](https://actualbudget.org/) is a local-first personal finance
app with an optional self-hosted sync server.

- [Documentation](https://actualbudget.org/docs/)
- [Helm Chart](https://github.com/community-charts/helm-charts/tree/main/charts/actualbudget)

## User namespaces

Production runs a [stateful user-namespace pilot](../../../providers/hetzner/apps/actual-budget/components/user-namespaces/README.md)
whose runtime verdict is recorded on
[#3604](https://github.com/devantler-tech/platform/issues/3604). Its component
pairs the pod setting with the existing admission policy and preserves the
database volume, both containers' identities and mounts, and the single-writer
deployment strategy. The guide covers offline validation, the runtime verdict,
and rollback. Local app opt-in keeps the shared defaults.

## Bank sync (Enable Banking)

Bank-sync credentials (Application ID + PEM secret key) live only in the
sync-server's internal `secrets` SQLite table — Actual exposes no env/config
hook for them. They are stored in OpenBao at `apps/actual-budget/enablebanking`
and reconciled into the table by the `enablebanking-seed` sidecar (see
[`config-map.yaml`](config-map.yaml)). The sync-server reaches the Enable
Banking API (`api.enablebanking.com`) egress-allowed by
[`cilium-network-policy.yaml`](cilium-network-policy.yaml).

A bank connection is a consent that Actual requests for at most 90 days (less
when the bank allows less). When it expires, every account on that connection
fails to sync with `Failed syncing account "<name>."`, and Actual marks the
accounts with a red dot. Re-authorising needs the account holder's bank login,
so it cannot be automated. In Actual, open one of the failing accounts, choose
to link it again through Enable Banking, log in to the bank, and map the bank's
accounts back onto the existing Actual accounts. Expect to repeat this about
every 90 days.

The sync server answers every bank-sync failure with HTTP 200, so the access
log does not show why a sync failed. First check the reason Actual shows when
you select the red dot. If that is not enough, the server can log the Enable
Banking API's error status and message. That output is bank data, which Actual's
[troubleshooting guide](https://actualbudget.org/docs/troubleshooting/server/)
treats as sensitive, and the cluster's log collector keeps container output for
seven days. So it stays off, and is turned on only for one reproduction:

1. In a pull request, add this post-renderer patch to
   [`helm-release.yaml`](helm-release.yaml), after the existing `env` patches:

   ```yaml
   - op: add
     path: /spec/template/spec/containers/0/env/-
     value:
       name: DEBUG
       value: "actual:enable-banking:errors"
   ```

2. After it deploys, sync once, then read the result:

   ```sh
   kubectl -n actual-budget logs deploy/actual-budget-actualbudget -c actualbudget | grep enable-banking
   ```

   An expired consent shows as a 401 or 403, or as a closed or expired session.
   Any other error points at the platform or at Enable Banking instead.
3. Remove the patch again in a follow-up pull request.

## End-to-end encryption (manual, by design)

Actual's end-to-end encryption **cannot be enabled declaratively**, and by
design it should not be: it is a **client-side, password-derived** feature. When
you turn it on, the client derives an encryption key from a password *locally*,
re-encrypts the budget, and uploads only ciphertext plus non-secret key metadata
(`keyId`, `keySalt`, an encrypted `testContent`). The **server never sees the
password or key** — that is precisely what makes it end-to-end. There is no
`ACTUAL_*` env var and no server endpoint to turn it on; the sync-server's
`/user-create-key` only *stores* metadata the client computed.

So enabling it is a **one-time manual step**, and what we manage declaratively is
the **password of record** (in OpenBao) so it survives DR:

1. Store the password in OpenBao at `apps/actual-budget/encryption`
   (property `password`). The path is seeded create-only with a placeholder by
   [`push-secret-seed-actual-budget-encryption.yaml`](../../infrastructure/vault-seed/push-secret-seed-actual-budget-encryption.yaml);
   type the real password in via the OpenBao UI/CLI. Nothing in-cluster reads it
   back — it exists only as the durable record of the password.
2. In the Actual web UI: open the budget → **Settings → Show advanced settings →
   Enable encryption**, and enter that same password.
3. Every device that opens the file will now prompt for the password once.

> [!CAUTION]
> A lost E2EE password means the budget data is **permanently unrecoverable** —
> the server cannot help, because it never had the key. Treat the OpenBao
> `apps/actual-budget/encryption` entry as a root of trust; custody guidance is
> in [`docs/dr/crypto-custody.md`](../../../../docs/dr/crypto-custody.md).
