# Cryptographic custody

This is the deep dive on the *cryptographic* artifacts that protect the
platform: how each one is generated, where it must be kept, what an
attacker who steals it could do, and what we do when it is lost.

The companion docs are [runbook.md](runbook.md) (scenarios &amp; commands)
and [openbao.md](openbao.md) (OpenBao-specific DR).

> A consistent risk model across the document: **possession of a private
> key ≈ full impersonation of whatever that key signs or decrypts.** All
> recommendations below follow from that.

---

## The roots of trust at a glance

| Root of trust              | Used to…                                                           | If a copy leaks                                              | If all copies are lost                                            |
| -------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------ | ----------------------------------------------------------------- |
| **SOPS Age private keys**  | decrypt every `*.enc.yaml` in this repo (per-env)                  | decrypt all committed secrets — rotate underlying values     | re-encrypt every `*.enc.yaml` with a new recipient                |
| **Talos cluster PKI**      | issue node certs, sign kubelet/etcd identities, bootstrap workers  | impersonate any cluster component                            | full cluster rebuild — re-runs of `ksail cluster create`          |
| **OpenBao unseal key**     | decrypt the OpenBao master key (which encrypts every KV entry)     | unseal OpenBao and read every secret                         | if a Velero snapshot still contains the `openbao-unseal` Secret, restore it (see [openbao.md](openbao.md) scenarios); if every copy is gone, re-initialize OpenBao and re-seed the KV (existing encrypted data is unrecoverable) |
| **OpenBao root token**     | bypass every OpenBao policy                                        | full control of OpenBao                                      | re-mint with the unseal key (`bao operator generate-root`); if both are gone, re-initialize OpenBao |
| **cosign signing identity**| sign the platform OCI artifact published to GHCR                   | publish artifacts the cluster will trust as ours             | re-sign on next CI run (no on-disk key — see below)               |
| **Hetzner Cloud token**    | provision / destroy nodes; reconfigure Cloud LB and firewall       | destroy infrastructure; pivot via the LB                     | mint a new one in the Hetzner console; rotate `HCLOUD_TOKEN`      |
| **Cloudflare API token**   | manage DNS for the platform domain                                 | redirect prod traffic; mint Origin CA certs                  | mint a new one in the Cloudflare dashboard                        |
| **Actual Budget E2EE password** | client-side key to encrypt/decrypt an Actual budget file      | decrypt that budget's financial data                        | the budget is **permanently unrecoverable** — the server never held the key |

The rest of this document walks each row in detail.

---

## SOPS Age keys

**What:** an X25519 keypair per environment, used by SOPS to wrap the
per-file data key in every `*.enc.yaml` matching the corresponding
`creation_rules` entry in [`.sops.yaml`](../../.sops.yaml).

Current recipients (public halves are in `.sops.yaml`):

- `age14skmde…ushfn5uq` — **local** (`k8s/clusters/local/**`)
- `age1rk6fs67…sk8wgs6` — **prod** (`k8s/clusters/prod/**`, `k8s/providers/hetzner/**`)

Both keys also wrap files that are not cluster-scoped (`k8s/bases/**/*.enc.yaml`,
`k8s/bases/bootstrap/*.enc.yaml`), so either key can decrypt those.

### Custody recommendations

1. **Primary copy:** developer workstation at `~/.config/sops/age/keys.txt`
   (this is where SOPS looks by default and what the `ksail` CLI uses).
2. **Hot backup:** a password manager you actually use (1Password, Bitwarden,
   the macOS Keychain), so a wiped laptop does not become a recovery
   incident.
3. **Cold backup:** offline. Two non-trivial options, in order of
   preference:
   - **Hardware-backed Age key via [`age-plugin-yubikey`](https://github.com/str4d/age-plugin-yubikey)** — a YubiKey 5 series generates the X25519 key on-device and never exports it; `.sops.yaml` records the `age1yubikey1…` identity. Loss of the YubiKey = key loss, so always provision *two* (primary + cold backup) and add both as recipients. SOPS supports any number of recipients.
   - **Printed paper** — `age-keygen` produces a 74-character private key. Print it on an air-gapped printer and store in a safe. Cheap, reliable, slow.
4. **GitHub Actions secret** (`SOPS_AGE_KEY` in the `prod` environment) —
   automatically populated; rotated whenever the key rotates (Scenario 6
   in [runbook.md](runbook.md)).

### Rotation hygiene

After every rotation, **run `sops updatekeys` on every encrypted file**.
Skipping a file leaves the retired recipient still wrapped in its data key,
and any holder of the retired private half can still decrypt that file. The
gitleaks workflow added in [PR #1616](https://github.com/devantler-tech/platform/pull/1616)
does not catch this — it's an Age-specific failure mode. There is a
maintenance script suggested in [runbook.md Scenario 6, step 6](runbook.md):

```sh
sops updatekeys --yes $(git ls-files '*.enc.yaml')
```

Run it as the final step of every rotation. If you forget, the periodic
audit script below catches it.

### Audit script (optional, run weekly)

```sh
# List every Age recipient currently embedded in any *.enc.yaml file.
# Anything that is NOT in .sops.yaml is a stale recipient and must be
# stripped with `sops updatekeys --yes <file>`.
for f in $(git ls-files '*.enc.yaml'); do
  grep -oE 'age1[a-z0-9]+' "$f"
done | sort -u
```

Compare to the `age:` values in `.sops.yaml`. Discrepancies = stale wraps.

---

## Talos cluster PKI

**What:** the Talos cluster CA (`machine.ca`) plus the per-node certificates
that derive from it. Used to authenticate the Talos API (`apid`),
`kube-apiserver`, etcd peers, and kubelets.

**Where it lives:** the `talosconfig` file written to `~/.talos/config` by
`ksail cluster create`. The PKI for nodes is embedded in each node's machine
config, generated by Talos at boot from a cluster secret bundle.

### Why this is a third root of trust

Even with a perfect SOPS setup, an attacker who steals the *talosconfig*
can:

- Read and modify any node's machine config (rotate kubelet certs, install
  arbitrary kernel extensions, exfiltrate etcd snapshots).
- Restart nodes into maintenance mode and reset machine secrets.

So the talosconfig is functionally equivalent to root on every node.

### Custody recommendations

The talosconfig is **regenerable** as long as the cluster exists — `ksail`
re-derives it from the cluster's running PKI on demand. So the custody
posture is:

1. **Do not commit it to git.** It is `.gitignore`d via the default
   `~/.talos/config` location.
2. **Keep one copy on each operator workstation** that needs to manage
   nodes. Replacing a workstation = re-run
   `./scripts/run-ksail-prod-with-pull-auth.sh cluster update`
   (which regenerates the local `~/.talos/config` from the cluster's
   running PKI), or use `talosctl config merge <talosconfig>` against
   a backup file. Talos does not have SSH-equivalent — see DR Scenario 4
   in [runbook.md](runbook.md) for the rebuild-from-zero path when no
   talosconfig copy exists anywhere.
3. **CI:** stored as the `TALOS_CONFIG` secret in the GitHub `prod`
   environment. Refreshed by [runbook.md Scenario 9](runbook.md) after
   any cluster rebuild.

### What to do if it leaks

A leak of the talosconfig is a **cluster-rebuild event**, not a rotation
event:

1. Provision a new cluster from scratch (`ksail cluster create` against a
   fresh Hetzner project, or a separate naming scheme).
2. Restore application data from Velero/CNPG (Scenarios 4–5).
3. Cut DNS over to the new cluster, then destroy the old one (so the leaked
   credentials lose their target).

There is no "rotate the cluster CA in-place" workflow for Talos; the PKI is
bound to the cluster secret bundle generated at `ksail cluster create` time.

### What to do if it is *lost* (no copies remaining)

Practically the same — rebuild from zero. But this is *much* easier than
the leak case because there is no time pressure to outrun the attacker, and
the existing cluster keeps serving traffic until DNS cuts over.

---

## OpenBao unseal key &amp; root token

This is covered in detail in [openbao.md](openbao.md); summary here for
completeness:

- **Unseal key** is a Shamir 1-of-1 share stored in the `openbao-unseal`
  Kubernetes Secret. Velero backs it up; an off-cluster copy in an
  operator vault is recommended.
- **Root token** is stored in the same Secret; it should be revoked after
  every cluster bootstrap and re-minted only on demand via the unseal key.
- Future direction (planned in Phase 1.4 of the hardening series): migrate
  to **transit auto-unseal** so the unseal key lives in a second, narrowly
  scoped Bao that is itself sealed by an HSM-style root. That eliminates
  the "one Secret = the whole vault" failure mode.

---

## cosign signing identity (keyless via Fulcio / Rekor)

**What:** the OCI artifact published to `ghcr.io/devantler-tech/platform/manifests`
by `cd.yaml` will (after Phase 2.1) be signed with cosign **keyless**
signing — Fulcio mints a short-lived certificate bound to the GitHub
Actions OIDC identity of this workflow, the signature is recorded in
Rekor, and the certificate's `Subject Alternative Name` is the workflow
URI (e.g. `https://github.com/devantler-tech/platform/.github/workflows/cd.yaml@refs/heads/main`).

**Implication:** there is no private key on disk. The root of trust is the
combination of:

1. **Fulcio's CA** — public, federated, audited.
2. **Sigstore's transparency log (Rekor)** — public, append-only.
3. **The `matchOIDCIdentity` regex** in the consumer-side `OCIRepository`
   `spec.verify` — this is the actual policy decision: "trust signatures
   that Fulcio issued to *this* workflow."

The `matchOIDCIdentity` regex is therefore the closest thing to a
"signing key" in this architecture. **Treat it as a security
configuration value with the same review discipline as
`.sops.yaml`.** A PR that loosens the regex (e.g. removes the `refs/tags`
constraint) should be reviewed as carefully as one that adds a new SOPS
recipient.

### Recovery scenarios

- **Workflow secret leaks:** there is no secret to leak — there is no
  signing key. The worst an attacker who steals a one-shot OIDC token
  could do is sign **one** artifact, and the signature would be recorded
  in Rekor with a timestamp. Detection is via the Rekor entry's
  timestamp + OIDC subject; **mitigation is on the consumer side** — pin
  Flux's `OCIRepository.spec.verify.matchOIDCIdentity` to a stricter
  pattern (e.g. include `refs/tags/v[0-9]+` to reject signatures from
  non-release runs) so the rogue artifact's signature no longer
  validates. Flux's verify schema does not natively support Rekor
  log-index exclusions; the policy lever you actually have is the
  `matchOIDCIdentity` regex and a `secretRef` containing trusted CA
  bundles.
- **GitHub identity gets compromised (the org / workflow itself):** the
  attacker can sign artifacts that will validate against the existing
  `matchOIDCIdentity`. Mitigation: tighten the regex (pin to a specific
  commit SHA range, e.g. `@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`), and
  pin the consumer `OCIRepository.spec.ref.digest` to a known-good
  release so future tag pushes from the compromised identity are
  ignored until the regex catches up.

The reference architecture for keyless cosign verification is documented
upstream at <https://docs.sigstore.dev/cosign/verifying/verify/>.

---

## Actual Budget end-to-end-encryption password

**What:** a user-chosen password from which the Actual Budget **client** derives
the key that encrypts a budget file. It is a fundamentally different kind of
secret from the rest of this document: it is **not** a platform-managed key and
the sync-server never possesses it. When E2EE is enabled, the client derives the
key locally, re-encrypts the budget, and uploads only ciphertext plus non-secret
metadata (`keyId`, `keySalt`, an encrypted `testContent`) — so the server holds
no material that can decrypt the data. That is what makes it *end-to-end*, and
it is also why there is no declarative "enable encryption" switch: turning it on
is a one-time client action (see the
[actual-budget app README](../../k8s/bases/apps/actual-budget/README.md)).

**Where it lives:** OpenBao at `apps/actual-budget/encryption` (property
`password`), seeded create-only with a placeholder by
`push-secret-seed-actual-budget-encryption.yaml`. Unlike every other secret in
this platform, **nothing in-cluster reads it back** — no ExternalSecret
materialises it — because only a client can use it. OpenBao is purely the
durable record so the password is not lost.

### Custody recommendations

Treat it like the SOPS Age keys: a primary copy in OpenBao, plus at least one
off-cluster copy in a password manager you actually use. It is short (a
human-typed passphrase), so a printed cold backup is trivial.

### What to do if it leaks

Rotate it: in the Actual UI, disable then re-enable encryption with a new
password (this re-encrypts the file), and update the OpenBao entry. Anyone who
captured the old ciphertext *and* the old password could decrypt the data as it
was, so also consider the exposed transactions compromised.

### What to do if it is *lost* (no copies remaining)

The budget file's server-side data is **permanently unrecoverable** — there is
no reset, no recovery, no operator override, because the platform never held the
key. The only fallback is an *unencrypted* copy of the budget that predates
encryption (a local device that still has it, or a pre-E2EE export). This is the
strongest reason to keep the OpenBao entry backed up.

---

## Cross-references

- [`runbook.md`](runbook.md) — operational scenarios (rotation steps, full
  rebuild).
- [`openbao.md`](openbao.md) — OpenBao-specific DR (seal/unseal,
  backup/restore).
- [`../secret-rotation.md`](../secret-rotation.md) — design of the
  rotation pipeline (OpenBao Database engine, static roles, ESO refresh).
- [`../../SECURITY.md`](../../SECURITY.md) — public-facing vulnerability
  disclosure policy.
