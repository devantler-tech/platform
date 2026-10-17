# Service session for scheduled bank sync: safety proof

This document is the safety proof for **path A** in
[#5027](https://github.com/devantler-tech/platform/issues/5027): a scheduled job that signs in to
Actual Budget with a service session while every person still signs in through SSO. It contains no
implementation. Path A ships only if review of this document shows it is still safe, and only after
the maintainer makes the decision in [section 5](#the-decision-only-the-maintainer-can-make).

Source references are to Actual at `v26.10.0`, the release this platform runs, under
`packages/sync-server/src/` unless a path says otherwise. The design being judged is the one in the
[investigation on #5027](https://github.com/devantler-tech/platform/issues/5027#issuecomment-6239074349).

## Path A in brief

- **A service user.** A non-admin user in Actual with exactly one budget shared with it.
- **A token.** A random value generated in the cluster and kept in OpenBao.
- **A session reconciler.** A sidecar in the Actual pod keeps exactly one session row for that user
  with that token. It writes Actual's account database directly, because Actual has no other way to
  issue a session without a person logging in.
- **A nightly job.** A CronJob signs in with the token through `@actual-app/api`'s `sessionToken`
  option, syncs each linked account, uploads the result and exits non-zero when any account fails.

Nobody logs in, so no login method is added or relaxed.

## 1. Necessity

### What is blocked

| Goal | Why it needs a scheduled, signed-in job |
| --- | --- |
| Nightly bank sync | Actual pulls bank transactions only when a signed-in client asks. The server has no schedule. The automatic bank sync on Actual's `master` is a timer in the web client that runs only while a device has the budget open. |
| Early warning of an expired bank consent | A consent lasts at most 90 days. When it expires, every account on it fails, and the server answers each failure with HTTP 200, so only a client that runs a sync can see it. |
| Private spend summary ([#5029](https://github.com/devantler-tech/platform/issues/5029)) | It has to read the budget on a schedule with no person present. |

### Evidence: the 2026-10-14 outage

- On 2026-10-14 bank sync stopped for every account, because the bank consent reached its 90-day
  limit.
- Every platform signal stayed healthy: the pod was ready, the seed sidecar reconciled, the
  ExternalSecrets synced and the egress policy allowed the provider. Nothing alerted, because
  nothing but a person ever runs a sync.
- It came to light only when the app was next opened, and was diagnosed in
  [#4932](https://github.com/devantler-tech/platform/pull/4932) on 2026-10-15.
- It recurs about 90 days after every re-link. With a nightly job, the next expiry becomes a failed
  Job on its first night, which the platform's CronJob failure alert
  ([#2915](https://github.com/devantler-tech/platform/issues/2915)) reports.

The lifestyle floor's outcome 4 reads: *the household finance app keeps working, privately, with
bank sync intact.* Both halves bind. The fix is needed for "bank sync intact", and this proof exists
because it must not cost "privately".

### Why nothing lighter works today

- **Turn off `ACTUAL_OPENID_ENFORCE`, or bring back password login.** Ruled out: it weakens SSO for
  people.
- **An API token or a service account.** Actual has neither, at 26.10.0 or on `master`.
- **An OpenID session without a person.** Actual's callback accepts only a sign-in flow that Actual
  itself started, so no grant Dex offers can complete it (finding 3 of the investigation).
- **A reminder before each 90-day limit.** Needs no credential and is worth having anyway, but it
  covers only the consent warning, not nightly sync or #5029.

### Why path B does not remove the need in the short term

Path B is an upstream contribution that would let an Actual admin issue scoped, revocable API
tokens.

- **The timing is not ours.** It needs agreement on the design with Actual's maintainers, review
  under their contribution and AI policies, a release, and then our upgrade. Actual releases monthly,
  so even a quick acceptance is a release or more away, and acceptance is not certain.
- **The next consent expiry does not wait for it.**
- **It removes only part of the risk.** An upstream token holder could read and change the shared
  budget exactly like path A's session, and any server-side sync still needs the end-to-end
  encryption password inside the cluster. Path B removes what is unique to path A
  ([section 2.6](#26-what-path-a-adds-beyond-any-scheduled-sync)): a credential created outside
  Actual's own code, the dependency on Actual's tables, and possibly the unlimited lifetime.

Path A is therefore a bridge, built to be retired when path B lands. Retiring it is the rollback in
[section 5](#5-rollback-and-residual-risk).

## 2. Threat model

### 2.1 Assets

1. **Budget data:** transactions, balances, payees and notes. Both confidentiality and integrity.
2. **The end-to-end encryption password:** it decrypts the budget wherever a copy exists, backups
   included.
3. **The bank connection:** the consent and the provider credentials that let the server pull fresh
   bank data.
4. **The SSO boundary:** every person signs in through Dex.
5. **Actual's account database** (`account.sqlite`): users, sessions (people's session tokens
   included), budget grants and the server password hash.

### 2.2 What the token holder can do

The token is a session of a `BASIC` user that owns nothing and has one budget shared with it.

| It can | Source at 26.10.0 |
| --- | --- |
| Read the whole shared budget. Without the encryption password this is ciphertext. | `app-sync.ts:128-136` (`requireFileAccess`) on the sync and download routes |
| Change anything in it: add, edit or delete transactions, accounts and categories, or replace the server copy with an upload. | `app-sync.ts` `/sync` and `/upload-user-file` (access check at `348`) |
| Rename the budget, and create new budget files of its own. | `/update-user-filename`, `/upload-user-file` |
| Ask the server for a linked account's balances and transactions from the bank, in plaintext, when it knows that account's provider id. End-to-end encryption does not cover this. | `app-enablebanking/app-enablebanking.ts:170`, `491` |
| Start bank syncs, which count against the bank's access limits. | the same routes |
| List every user's name. | `app-admin.js:29-45`, open to every session by design |
| Overwrite the bank-sync provider credentials. A known gap at 26.10.0, already closed on `master`. | `app-enablebanking/app-enablebanking.ts:215-262` |

| It cannot | Source at 26.10.0 |
| --- | --- |
| Sign in as anyone else, or create a session. | Only the password login and the OpenID callback create sessions. |
| Reach an admin route: user management, server settings, password change or global secrets. | `app-admin.js:47-185`, `app-account.js:131-186`, `app-secrets.js:19-34`, `68-75` |
| Share the budget, remove someone's access or transfer ownership. | Owner or admin only: `services/user-service.ts:172-181`, `app-admin.js:187-415` |
| Delete or reset the budget, or change its encryption key. | Owner or admin only: `app-sync.ts:119-126`, `232-296`, `544-572` |
| See a budget that is not shared with it. | `app-sync.ts:128-136` |

### 2.3 Token lifetime

- The session row has `expires_at = -1`, so it never expires, and Actual's cleanup of expired
  sessions skips it (`account-db.js:275-284`). People's sessions follow the Dex token
  (`ACTUAL_TOKEN_EXPIRATION=openid-provider`).
- The token stops working when:
  - the user is disabled. `getSession` accepts only enabled users (`account-db.js:221-230`), so
    this takes effect at once;
  - the user is deleted. Actual deletes its sessions with it (`services/user-service.ts:87-98`);
  - a new token is written to OpenBao. The old one stops within one ExternalSecrets refresh (1
    hour) plus one reconciler pass (5 minutes);
  - the OpenBao value is set to `PLACEHOLDER`.
- A leaked token therefore lasts until someone rotates or disables it. Actual keeps no audit trail of
  which session made a request, so a leak is hard to see. Scheduled rotation (control C4) is what
  bounds it.

### 2.4 The encryption password becomes readable in the cluster

- Today nothing in the cluster reads it: [`docs/dr/crypto-custody.md`](../../../../../docs/dr/crypto-custody.md)
  records that OpenBao holds it only as the value of record.
- Any server-side sync, path A or B, has to decrypt the budget, so the job needs the password. It
  becomes a Kubernetes Secret in `actual-budget` and a file in the job's pod. While the job runs, the
  decrypted budget is in the pod's memory and data directory.
- Who could then decrypt the budget:
  - anyone who can read Secrets in `actual-budget`;
  - whoever controls the node that runs the job, while the job's pod exists, since a node can read
    the Secrets its own pods reference;
  - anyone who can read a backup that contains the Secret (attacker E below).
- The custody document has to change in the same pull request as the implementation, and the
  maintainer has to accept this explicitly.

### 2.5 Attackers

| Attacker | Gains today | Extra gain with path A | What bounds it |
| --- | --- | --- | --- |
| **A. A compromised job pod**, through a bug or a malicious pinned package | Nothing: no job exists. | The token, the encryption password and the decrypted budget for one run, so it can read and change the budget. | Egress only to Actual's port (C3). No service-account token, non-root, read-only filesystem (C6). The pod lives minutes. Dependencies are integrity-pinned. With packaging option 1 the pod can also reach the npm registry, a way out (R5). |
| **B. A compromised node** | On the node running Actual: the account database, the provider credentials, the OIDC client secret and the encrypted budget. | On the node running the job, during the run: the token and the encryption password, so the budget becomes readable. | The encryption password has its own Secret that only the job references (C2), so the always-running Actual pod's node never receives it. The job runs for minutes a night. |
| **C. A leaked OpenBao path** | `apps/actual-budget/encryption` already holds the password. It is useless without the ciphertext. | `apps/actual-budget/bank-sync` yields the token. Token, password and network reach together give full read and write. | The token works only from the job's pods or behind the SSO gate (C3, C7). Rotation (C4). The OpenBao access policy is unchanged. |
| **D. Log exposure** | The log collector keeps container output for 7 days. Actual's request log prints only method, status and URL (`util/middlewares.ts:47-61`), so the `X-ACTUAL-TOKEN` header is never logged. | Actual's client library logs account names when a sync fails, even with `verbose: false` (finding 4 of the investigation). | The job silences the console and prints counts and error codes only (C5, T5). |
| **E. Backup exposure** | The daily Velero backup copies the namespace's Secrets and its data volume to the off-site store: every namespace except `kube-system` and `velero`, kept 14 days ([`helm-release.yaml`](../../../infrastructure/controllers/velero/helm-release.yaml)). | It would also copy the token and the encryption password next to the encrypted budget, so whoever reads the store could decrypt it offline. | Both new Secrets are excluded from backups (C2). OpenBao stays the value of record, and ExternalSecrets recreates them after a restore. A restored session row is harmless, because the reconciler deletes every session except the configured one on its first pass. |
| **F. Another workload in the cluster** | It cannot reach Actual, since only the auth proxy may connect, and cannot read its Secrets. | None, if the new ingress rule admits only the job's pods (C3). | Anyone who can create pods in `actual-budget` can already read its Secrets, so a label-based rule gives them nothing new. |

### 2.6 What path A adds beyond any scheduled sync

1. A credential that never passed SSO, created by writing Actual's database directly.
2. A dependency on the shape of Actual's `users` and `sessions` tables.
3. A third container with write access to the account database. The existing seed sidecar already
   has the same access.
4. A session that never expires.

## 3. Compensating controls

Each control says what it is, what it stops, and which tests in
[section 4](#4-acceptance-tests) prove it.

### C1. A non-admin user with only this budget shared

- The user is `BASIC`, owns nothing, and is named `bank-sync@actual-budget.invalid`. A GitHub login
  cannot contain `@` and no verified email ends in `.invalid`, so no person can sign in as it through
  Dex.
- The reconciler refuses to start unless its configured user name has that shape, so a
  misconfiguration cannot attach a never-expiring session to a person's account. This is a change to
  the investigated script, which accepts any name.
- If the user is ever made an admin or an owner, the reconciler deletes its sessions and stops.
- Its only grant is the one budget the maintainer shares by hand under User Access. The job fails
  when it sees anything other than exactly one budget, so an extra share shows up the same night.
- Tests: T1d, T3, T7.

### C2. A token generated in the cluster, kept in OpenBao, delivered by ExternalSecrets

- An ExternalSecrets `Password` generator creates a 64-character alphanumeric token, and a
  PushSecret writes it to OpenBao at `apps/actual-budget/bank-sync`. No person or agent ever sees it.
- Two Secrets, not one:
  - the session token, mounted by the reconciler sidecar and the job;
  - the encryption password, mounted by the job only. The Actual pod never references it, so the
    node running Actual never receives it.
- Both are mounted as read-only files, never as environment variables, and both carry the label
  `velero.io/exclude-from-backup: "true"`.
- The reconciler ignores the `PLACEHOLDER` sentinel and rejects a token of unexpected shape.
- Tests: T6.

### C3. A network policy that lets the job reach only the Actual service

Cilium policies add up, and the existing [`cilium-network-policy.yaml`](../cilium-network-policy.yaml)
selects every pod in the namespace (`endpointSelector: {}`), so a new pod there would inherit its
Dex and bank-provider egress. The implementation therefore:

1. narrows the existing policy to the Actual pod's labels;
2. adds a policy for the job's pods that allows egress only to the Actual pod's port and to DNS,
   plus the npm registry by name with packaging option 1 only;
3. adds one ingress rule to the Actual pod that admits the job's pods, by label, from
   `actual-budget` only.

No other ingress changes: the auth proxy stays the only other source. Tests: T1b, T8.

### C4. Rotation and revocation, by deleting the session

- **Rotate.** Write a new token to OpenBao. The reconciler installs the new session and deletes
  every other session of the user, so the old token stops within one refresh plus one pass, about
  65 minutes. Rotate on a schedule, every 30 days, through the generator's refresh. Before relying
  on that, confirm at the deployed ExternalSecrets version that a refresh generates a new value.
- **Revoke now.** Disable the user in Actual's User Directory. Requests with its token fail at once,
  and the reconciler deletes its sessions on its next pass. It never re-enables a disabled user.
- **Revoke for good.** Delete the user. Actual deletes its sessions and its grant. The reconciler
  must then **not** recreate it: it records, in a marker file on the data volume and outside
  Actual's tables, that it created the user once, and refuses to create it again. This is a change
  to the investigated script, which recreates a missing user on its next pass. Unchanged, a deleted
  user would come back within 5 minutes with a valid session: without the budget grant, but with
  every other capability in [section 2.2](#22-what-the-token-holder-can-do).
- Tests: T4.

### C5. Logs carry counts only

- The job silences every console method and prints one line, `linked=N synced=N failed=N codes=…`,
  or one of its fixed failure messages. An error code is printed only when it matches a short code
  pattern, because an error message can name an account.
- The reconciler prints only fixed messages and never the token.
- Actual's own `DEBUG` logging for bank sync stays off, as the [app README](../README.md) requires.
- Tests: T5.

### C6. A short-lived, single-purpose run

- One nightly CronJob that only syncs:
  - `concurrencyPolicy: Forbid`;
  - `backoffLimit: 0`, so it fails loudly once instead of retrying against the bank's access limits;
  - an `activeDeadlineSeconds` bound;
  - `ttlSecondsAfterFinished`, so the pod and its Secret mounts go away soon after the run.
- Its own ServiceAccount with no RBAC and `automountServiceAccountToken: false`. It runs non-root
  with a read-only root filesystem, all capabilities dropped and the `RuntimeDefault` seccomp
  profile, like the existing sidecar.
- The decrypted budget lives in a memory-backed `emptyDir` with a size limit, so plaintext never
  reaches the node's disk and disappears with the pod.
- The platform's CronJob failure alert watches it, so a failed sync, consent expiry included,
  raises an alert after the first failed night.
- The spend summary in #5029 gets its own service user and token, so each can be revoked alone.
  Actual has no read-only share, so that user can change the budget too.
- Tests: T9.

### C7. No new route

- The job talks to the in-cluster Service. There is no new HTTPRoute, Gateway listener or exposed
  Service, and no change to the auth proxy or Dex.
- From outside the cluster the token is usable only after the existing maintainer-team SSO gate.
- Tests: T1a.

## 4. Acceptance tests

Each test names how it runs:

- **Static:** in CI, over the rendered manifests (`kubectl kustomize`). Automatable.
- **Unit:** in CI, against an in-memory or throwaway SQLite database and a fake API. Automatable.
- **Server:** in CI, against the pinned Actual server image with a throwaway data directory and no
  cluster. Automatable.
- **Live:** against production after a deploy. Either a one-shot probe Job that mounts the token
  itself and prints only route names and status codes, or a person. Manual until that probe exists.
  No person ever reads the token.

Path A ships only when every test below passes, apart from the named exception in T3.

### T1. Every human login still requires SSO

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T1a | The rendered Actual HelmRelease and the namespace's routes | `ACTUAL_OPENID_ENFORCE=true`, `ACTUAL_LOGIN_METHOD=openid` and `ACTUAL_ALLOWED_LOGIN_METHODS=openid` are set and `DEBUG` is not. The namespace has only its existing HTTPRoute, whose only backend is the SSO proxy, and no `LoadBalancer` or `NodePort` Service. | Static, automatable |
| T1b | The Actual pod's ingress allow-list | The policies that select the Actual pod admit exactly two sources: the auth proxy, and pods with the job's label in `actual-budget`. Any other `fromEndpoints`, `fromEntities` or `fromCIDR` entry fails the test. | Static, automatable |
| T1c | `GET /account/login-methods` | Returns exactly one method, `openid`. | Server, automatable; live, manual |
| T1d | The reconciler's user name | It refuses to start unless the name matches `^[a-z0-9-]+@[a-z0-9-]+\.invalid$`. | Unit, automatable |
| T1e | Signing in from a private browser window | The browser is sent to SSO. After SSO, Actual offers only OpenID and shows no password field. | Live, manual |

### T2. A password login request is refused

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T2a | `POST /account/login` with `{"loginMethod":"password","password":"<32 random characters>"}` | The response has no `data.token`, and its `reason` is not `invalid-password`. That reason would mean the server ran the password check, so the password method is still live. With `openid` as the only allowed method, the server falls back to OpenID instead: without a return URL it answers `Invalid redirect URL`. | Server, automatable; live, manual |
| T2b | The same request with `"loginMethod":"header"` and an `x-actual-password` header | The response has no `data.token`. | Server, automatable |
| T2c | The server log for those requests | It shows `Logging in via openid`. | Server, automatable |

**T2a is expected to fail today, whether or not path A ships.** Enforcement only hides the other
methods from the login screen (`account-db.js:32-46`). The login route still runs any method a
request names, as long as that method is allowed and configured (`account-db.js:61-72`), and the
allowed list defaults to all three (`load-config.js:129-134`). So if a server password is set, a
request carrying it can receive a session for the server's built-in admin user. The public route
reaches Actual only after the SSO gate, which limits who can try. Setting
`ACTUAL_ALLOWED_LOGIN_METHODS=openid` closes it, and it must ship in its own pull request before
path A.

That change removes today's break-glass login. When Dex is down, the public route is already closed
by the SSO gate, so the password login is reachable only from inside the cluster. After the change,
break-glass is a reviewed pull request that allows `password` again for the duration. Devices that
already hold the budget keep working offline in the meantime, since Actual is local-first.

### T3. The service session cannot reach any admin route

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T3a | Every route mounted in `app.ts:59-73`, called with the service session | Every route that succeeds is on the allow-list below. Every other route refuses it: 401 or 403, or a 400 whose reason is `file-denied`. | Server, automatable |
| T3b | The same route list, against production | Same result as T3a. | Live, manual until the probe exists |
| T3c | The reconciler, with the user made an owner or an admin | It deletes the user's sessions and refuses to continue. | Unit, automatable |

The allow-list:

- `/account/validate`, `/account/needs-bootstrap` and `/account/login-methods`;
- `GET /admin/users/` and `GET /admin/owner-created/`;
- the file-access routes of `/sync` for the shared budget: `sync`, `download-user-file`,
  `upload-user-file`, `get-user-file-info`, `list-user-files`, `user-get-key` and
  `update-user-filename`;
- the bank-sync routes the job's own run calls, which the test records from a run against a fake
  provider.

Writing it as an allow-list means a route added in a later Actual release fails the test until
someone classifies it. Routes the source shows must be refused include `POST`, `PATCH` and
`DELETE /admin/users`, `POST /account/change-password`, `POST /account/server-prefs`, `POST /secret`
without a file id, `POST` and `DELETE /admin/access`, `POST /admin/access/transfer-ownership/`, and
`/sync/user-create-key`, `/sync/reset-user-file` and `/sync/delete-user-file` for the shared budget.

**Known exception at 26.10.0:** `POST /enablebanking/configure` accepts any signed-in session. The
test records it as an expected failure that becomes a required refusal in the first release with the
admin check, which `master` already has. Until then, the seed sidecar restores the real credentials
within 5 minutes. The test classifies the configure routes of the other bank-sync providers too,
rather than assuming them.

### T4. Deleting the user ends all access

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T4a | Delete the user through Actual's admin route, then wait one reconciler pass | The token is refused at once with `token-not-found`, and is still refused after the pass, because the reconciler did not recreate the user. | Server and unit, automatable |
| T4b | Disable the user | The token is refused at once. After one pass the user has no sessions, and the reconciler never re-enables it. | Unit, automatable |
| T4c | Set the OpenBao value to `PLACEHOLDER`, or remove it | Every session of the service user is deleted, and no user is created. | Unit, automatable |
| T4d | Rotate the token | After one pass the old token is refused and the user has exactly one session. | Unit, automatable |
| T4e | At go-live and after each manual rotation: disable the user and run the job once | The job fails with its token-rejected message. Enabling the user again is a deliberate step by a person. | Live, manual |

**T4a fails against the investigated script**, which recreates a missing user on its next pass. It
needs the change described in C4. T4b to T4d are covered by the investigation's local tests.

### T5. No transaction data is logged

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T5a | Run the job against a fake API seeded with marker strings in payee, account, budget and note names, including failing accounts whose errors carry the account name | Every output line matches `<timestamp> [bank-sync] linked=<n> synced=<n> failed=<n>`, with an optional `codes=<code>x<n>,…` suffix, or is one of the job's fixed `FAILED:` messages. No marker string appears anywhere. | Unit, automatable |
| T5b | The reconciler's output, under every rule | Each line is one of its fixed messages. | Unit, automatable |
| T5c | The server's log during a fixture sync | It holds only request lines of the form `<timestamp> <level>: <METHOD> <status> <url>`, and no URL carries any value other than a file id. | Server, automatable |
| T5d | The first production runs | Every line the job printed matches the T5a pattern. | Live, automatable as a log query; manual at go-live |

The check is an allow-list of what a line may look like, not a search for known bad words, so a new
kind of leak fails it too. The investigation's local tests already show no account, budget or token
text reaching the log.

### Supporting tests

| ID | Check | Passes when | Mode |
| --- | --- | --- | --- |
| T6 | The two new Secrets | Both ExternalSecret targets carry `velero.io/exclude-from-backup: "true"`, and only the CronJob's pod template references the encryption-password Secret. | Static, automatable |
| T7 | The job with zero or two budgets visible | It exits 1. | Unit, automatable |
| T8 | The job pod's egress | The policies that select the job's pods allow only the Actual pod's port and DNS, plus the npm registry by name with packaging option 1. From the job's pod, any other in-cluster service times out. | Static, automatable; live, manual |
| T9 | The CronJob spec | `concurrencyPolicy: Forbid`, `backoffLimit: 0`, a deadline and a TTL, no service-account token, a memory-backed data directory and the hardened security context. | Static, automatable |

## 5. Rollback and residual risk

### Rollback

**Stop it now,** in minutes and without a pull request: disable the service user in Actual's User
Directory. Every request with its token fails at once. Suspend the CronJob through a pull request if
it should stop reporting failures.

**Remove path A,** in this order:

1. Disable the service user, if it is not disabled already.
2. Revert the implementation pull request: the CronJob, the sidecar, both ExternalSecrets, the
   PushSecret and generator, and the policy changes. The reconciler's marker file can stay on the
   data volume: it only stops a later reinstall from recreating the user unasked.
3. Delete the service user in Actual, which deletes its sessions and its grant. Reverting the
   manifests alone is not enough: the never-expiring session row would stay in Actual's database,
   valid for as long as the user exists and is enabled.
4. Delete `apps/actual-budget/bank-sync` from OpenBao.
5. Restore the custody document's statement that nothing in the cluster reads the encryption
   password.
6. If the encryption password may have been exposed, rotate it as the custody document describes.
   That re-encrypts the budget.

If a misused token changed the budget, restore it from a Velero backup (kept 14 days) or from a
device that still holds a good copy: the same recovery as for any bad sync.

**Retiring path A when path B lands** follows the same steps, with the job moved to an
upstream-issued token in the pull request of step 2.

### Residual risk

- **R1.** The encryption password is readable inside the cluster, by anyone who can read Secrets in
  `actual-budget` and by whoever controls the node that runs the job while it runs. No server-side
  sync avoids this; path B carries it too.
- **R2.** A never-expiring bearer token that never passed SSO. Where it works is bounded (the job's
  pods, or behind the SSO gate), and so is how long it lasts (rotation), but Actual keeps no record of
  which session did what, so misuse is hard to see.
- **R3.** The token alone can ask the bank, through the server, for a linked account's balances and
  transactions, outside end-to-end encryption, when it knows the account's provider id. At 26.10.0
  it can also overwrite the bank-sync provider credentials for up to 5 minutes. People's sessions
  can do the same today.
- **R4.** Path A writes Actual's `users` and `sessions` tables directly, so an upgrade that changes
  them breaks the reconciler. It fails loudly, since the job's sign-in is rejected, but every Actual
  upgrade must check those tables, as it already must for the seed sidecar's `secrets` table.
- **R5.** With packaging option 1 the job installs `@actual-app/api` from the npm registry at run
  time, so a pod that holds the token and the encryption password can reach the internet.
  Integrity pins stop a substituted package, not a malicious version that is already pinned.
  Option 2, a small first-party image built in CI, removes that egress.
- **R6.** The token can change the budget. Only the maintainer would notice tampering, and recovery
  comes from backups or devices.
- **R7.** T2a fails today, independent of path A, until `ACTUAL_ALLOWED_LOGIN_METHODS=openid`
  ships.

### The decision only the maintainer can make

Path A is the only way to reach all three goals today. With the gates below, it adds a bounded,
revocable risk on top of the one every scheduled sync carries (R1). Choose one:

1. **Accept path A with its gates.** You accept R1 and the custody document's change, and a
   non-person session that rotates every 30 days (R2). It ships only after:
   - `ACTUAL_ALLOWED_LOGIN_METHODS=openid` with a written break-glass procedure (T2);
   - the two reconciler changes in C1 and C4 (T1d, T4a);
   - separate Secrets excluded from backups, and the narrowed network policy (T6, T8);
   - every test in section 4 passing, apart from the named exception in T3.

   You also choose packaging option 1, accepting R5, or option 2.
2. **Defer to path B.** Keep syncing by hand, add a date-based reminder before each 90-day consent
   limit, and leave #5029 blocked until Actual offers a token.
3. **Reject scheduled sync.** Keep everything as it is.

**Recommendation:** option 1 with packaging option 2. The image is a one-time cost, and it means the
pod that holds both secrets has no way out of the cluster.

## 6. The permission-rule change the implementation needs

This section is for the Agent Improver, under
[devantler-tech/monorepo#4288](https://github.com/devantler-tech/monorepo/issues/4288).

**What was refused.** Writing path A's implementation into this repository: a script that inserts a
`sessions` row for a service user into Actual's account database, and the manifests that deploy it.
The permission layer classed it as a security weakening
([refusal record](https://github.com/devantler-tech/platform/issues/5027#issuecomment-6239088656)).
The refusal stands until this proof is reviewed and the maintainer decides.

**The narrowest allowance that unblocks it:**

- **Who:** an interactive session that the maintainer directs. Not a scheduled run.
- **When:** only after this document's pull request has merged with a green review, and the
  maintainer has chosen option 1 in his own comment on #5027.
- **Where:** one draft pull request in `devantler-tech/platform`, from a `claude/` branch whose name
  ends in `-5027`, changing only:
  - new files in `k8s/bases/apps/actual-budget/` for the reconciler, the CronJob and its script, the
    two ExternalSecrets, the job's network policy and the tests;
  - that folder's `helm-release.yaml` (one sidecar and its volumes), `cilium-network-policy.yaml`
    (C3), `kustomization.yaml` and `README.md`;
  - one PushSecret and one `Password` generator in `k8s/bases/infrastructure/vault-seed/` for
    `apps/actual-budget/bank-sync`;
  - the Actual section of `docs/dr/crypto-custody.md`;
  - the CI wiring for the tests in section 4.
- **What the code may do:** create or update only the one `users` row for the `.invalid` service
  user (role `BASIC`, owning nothing), and only that user's `sessions` rows with
  `auth_method = 'service'`. It never touches `auth`, `user_access`, `files`, `secrets` or another
  user's rows, and never logs the token. Key the allowance on what the code writes, not only on
  file paths: a path-only allowance would also admit a script that writes an admin session.
- **Still refused, unchanged:**
  - any change to `ACTUAL_OPENID_*`, `ACTUAL_LOGIN_METHOD` or the allowed login methods, other than
    narrowing them to `openid`;
  - any change to a route, the Gateway, a Service's exposure, the SSO proxy or Dex;
  - any network rule that admits another source;
  - reading any Secret value, or changing anything in the cluster;
  - promoting or merging outside the normal review and merge gates;
  - using the allowance for any other app.
- **Lifetime:** one use. Remove it when that pull request merges or closes.

**Evidence for it:**

- the refusal, and the decision to stop rather than work around it, recorded on #5027;
- the design and its local tests, in the
  [investigation](https://github.com/devantler-tech/platform/issues/5027#issuecomment-6239074349);
- this document and the outcome of its review;
- the maintainer's decision, once he records it on #5027.

**The route that needs no permission change:** the maintainer writes or commits the implementation
himself from this design, and the agents review it and drive it to merge like any pull request.
