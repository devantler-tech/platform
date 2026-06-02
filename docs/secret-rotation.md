# Secret Rotation Design

Status: **proposal** (2026-05-27). Goal: automated rotation of platform secrets,
preferring **OpenBao-native** mechanisms. This document is the design; each phase
ships as its own reviewed PR.

## Current state

- **OpenBao = KV v2 + Kubernetes auth only** (configured by the `vault-config`
  Job). No Database secrets engine is enabled yet.
- **Two sourcing paths coexist (mid-migration):**
  - **OpenBao → ESO**: generators (`k8s/bases/infrastructure/vault-seed/generators.yaml`) seed OpenBao KV
    once; `ExternalSecret`s sync to consumer namespaces (1h refresh). Used by
    fleetdm DB/redis/license, headlamp/actual-budget OIDC, cloudflare token, R2,
    alertmanager.
  - **SOPS → Flux postBuild substitution**: `${dex_client_secret}`,
    `${flux_web_client_secret}`, `${oauth2_proxy_cookie_secret}` etc. are still
    read from `k8s/clusters/*/bootstrap/variables-cluster-secret.enc.yaml`. **Dex
    (the OIDC provider) and oauth2-proxy read from here, not OpenBao.**

### Validated finding — `refreshInterval` does **not** rotate generators

The `k8s/bases/infrastructure/vault-seed/push-generated-secrets.yaml` PushSecrets
use `refreshInterval: "0"`, and the comment implies a non-zero value would rotate.
**It would not.** ESO **v2.5.0**'s
[PushSecret reconciler](https://github.com/external-secrets/external-secrets/blob/v2.5.0/pkg/controllers/pushsecret/pushsecret_controller.go)
persists a `GeneratorState` (statemanager) and reuses the prior state, keeping
generator output **stable** across reconciles. Empirically: generated values have
been unchanged for days and there are no live `GeneratorState` instances.
**Conclusion: rotation needs an explicit mechanism, not a config flip.**

## Feasibility by secret class

| Class | Secrets | OpenBao-native fit | Plan |
| --- | --- | --- | --- |
| **Database creds** | fleetdm MySQL `fleet` user, Redis | ✅ **Database engine, static roles** | Phase 1–2 below |
| **Internal random** | oauth2-proxy cookie secret | ❌ no native engine | Migrate consumer to OpenBao, rotate via scheduled Job; no shared party → only forces re-login |
| **Shared OIDC** | `dex_client_secret`, `flux_web_client_secret` | ❌ no native engine | Unify Dex + clients on OpenBao first; coordinated rotation (short consumer refresh) to avoid auth-mismatch skew |
| **Provider tokens** | cloudflare, hcloud, github, R2 | ❌ external system-of-record | Scheduled job calling provider API → write OpenBao; mostly out of scope |
| **Roots** | SOPS Age key, OpenBao unseal/root | ❌ | Manual runbook + calendar reminder |

## OpenBao-native rotation (chosen mechanism)

OpenBao's **Database secrets engine** supports **static roles** for MySQL/MariaDB
and Redis (via the Valkey-compatible plugin): OpenBao stores and **automatically
rotates the password of an existing database user** on a `rotation_period`. This
fits apps that read a credential from a `Secret` at startup (like fleetdm) —
unlike *dynamic* roles, which mint ephemeral users the app would have to re-fetch.

### Phase 1 — fleetdm MySQL `fleet` user

1. **Enable + configure the engine** (in the `vault-config` Job, idempotent):
   - `bao secrets enable database` (mount `database/`).
   - Configure a `mysql` connection plugin pointing at `fleetdm-mysql.fleetdm:3306`,
     authenticating with the **root** credential already in KV
     (`apps/fleetdm/mysql` → `mysql-root-password`). Restrict `allowed_roles` to
     `fleet`.
   - Create static role `fleet`: `username=fleet`, `rotation_period=720h` (30d).
     **Target the app user, never root** (OpenBao does not distinguish root when
     rotating).
2. **Consume the rotated credential** — *not* a plain `ExternalSecret`. ESO's
   Vault provider supports **KV only**
   ([docs](https://external-secrets.io/latest/provider/hashicorp-vault/): *"The
   KV Secrets Engine is the only one supported by this provider"*), so the
   database engine must be read via the
   **`VaultDynamicSecret` generator** (`generators.external-secrets.io`). A
   generator does a GET on `database/static-creds/fleet` and returns the `data`
   map; an `ExternalSecret` consumes it via `dataFrom.sourceRef.generatorRef` and
   maps the `password` field into the `mysql` Secret's `mysql-password` key. Keep
   `mysql-root-password` / `mysql-replication-password` on KV (root rotation is
   out of scope; never put root under a static role).
3. **Propagation**: the ExternalSecret's `refreshInterval` re-reads the current
   static cred; Reloader restarts the fleet pods when `mysql-password` changes.
   Use a **short** refreshInterval (~1m) so the post-rotation window (below) is
   small.

### Credential handover sequence

- **Fresh cluster** (Flux order `bootstrap → infra-controllers → infra → apps`):
  KV seeds → ESO writes `mysql` Secret → bitnami MySQL bootstraps the `fleet`
  user with that password → `vault-config` enables the DB engine and creates the
  static role → OpenBao performs the **initial rotation** of `fleet` → ESO
  updates the `mysql` Secret → Reloader restarts fleet with the new password.
- **Existing cluster** (current prod): same, but the static role's first rotation
  changes the live `fleet` password immediately. Ordering must ensure the DB
  engine config runs **after** MySQL is reachable.

### Risks & rollback

- **Re-read behavior — VALIDATED (2026-05-27, isolated kind spike, ESO v2.5.0).**
  Unlike the PushSecret + Password generator (which *caches* via `GeneratorState`,
  per v2.5.0 source), the **`VaultDynamicSecret` generator re-reads on every
  ExternalSecret refresh**: changing the source value propagated to the synced
  Secret within one refresh cycle (~10s). So rotation **does** propagate — when
  OpenBao rotates the static-role password, the generator re-reads it and ESO
  updates the consumer Secret. No silent-rotation time-bomb.
- **Post-rotation window**: fleet reads its password once at startup, so after
  every rotation there is a brief window (≈ ExternalSecret refreshInterval +
  pod restart) where existing/new DB connections use the stale password and fail
  until Reloader restarts fleet. Inherent to a non-Vault-native app; bounded by a
  short refreshInterval + a long `rotation_period`. Acceptable for fleetdm.
- **Risk**: a misconfigured connection/role rotates `fleet` to a value the
  `Secret` doesn't reflect → fleet API loses DB access. **Mitigation**: validate
  the full chain on the local Talos+Docker cluster (CI system test exercises DB
  engine + ESO + fleet bring-up) before any prod tag.
- **Rollback**: point the `mysql` ExternalSecret back at KV
  (`apps/fleetdm/mysql`), then manually reset the `fleet` password to the KV
  value (`ALTER USER`). The DB engine mount can stay (unused) or be disabled.
- **Never** put root under a static role.

### Validation

- `kubectl kustomize k8s/clusters/local/` and `kubectl kustomize k8s/clusters/prod/`
  both build; `ksail workload validate` and `ksail --config ksail.prod.yaml workload validate`.
- CI's full local-cluster system test must bring fleetdm up healthy with creds
  sourced from `database/static-creds/fleet`.

### Phase 2 — fleetdm Redis

Same pattern using OpenBao's Valkey-compatible plugin (Redis-protocol) and a
static role for the redis user, once Phase 1 is proven.

## Non-database secrets (not OpenBao-native)

- **oauth2-proxy cookie secret** — first migrate the consumer from the SOPS Flux
  var to an OpenBao `ExternalSecret`, then rotate on a cadence via a small
  scheduled Job that writes a fresh random value to the KV path. No shared party
  → the only effect is forced re-login. (Cleanest non-DB rotation; can ship
  independently of the DB work.)
- **OIDC client secrets** — require unifying Dex (provider, currently SOPS) and
  all clients onto OpenBao, then rotating with a short consumer `refreshInterval`
  (or a coordinated job) so Dex and clients converge before the old secret is
  invalid; otherwise rotation causes an auth-mismatch outage.
- **Provider tokens / roots** — manual or provider-API driven; document a runbook
  and a calendar reminder for the SOPS Age key and OpenBao unseal/root token.

## Rollout order

1. Phase 1 — fleetdm MySQL static-role rotation (this design's flagship).
2. Phase 2 — fleetdm Redis static-role rotation.
3. oauth2-proxy cookie secret migration + scheduled rotation.
4. Later — OIDC coordination; provider-token jobs; root-secret runbook.
