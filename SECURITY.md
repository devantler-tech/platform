# Security Policy

## Supported versions

This repository is a continuously-deployed GitOps platform; only the `main`
branch and the most recent `v*` tag deployed to production are supported.
Older tags are kept for history and disaster-recovery purposes only and do
not receive security backports.

## Reporting a vulnerability

**Do not open a public issue for security reports.** Please use one of the
following private channels:

1. **Preferred** — open a private security advisory via GitHub:
   <https://github.com/devantler-tech/platform/security/advisories/new>.
   This keeps the report inside the repository's coordinated-disclosure
   workflow and gives us a private space to discuss and patch.
2. As a fallback, email <nikolaiemildamm@icloud.com> with `[platform-security]`
   in the subject line.

When you report, please include:

- A description of the issue and the impact you believe it has.
- Steps to reproduce, ideally with a minimal proof of concept.
- The commit SHA or release tag the report applies to.
- Any suggested remediation, if you have one.

## Response targets

We are a single-maintainer project; responses are best-effort but we aim for:

| Severity | First acknowledgement | Triage decision | Fix or mitigation |
| -------- | --------------------- | --------------- | ----------------- |
| Critical | 48 hours              | 5 days          | 14 days           |
| High     | 5 days                | 14 days         | 30 days           |
| Medium   | 14 days               | 30 days         | next minor        |
| Low      | 30 days               | next minor      | next minor        |

Severity follows the [CVSS v3.1] vector in the report; in case of
disagreement the maintainer's assessment is used and recorded in the
advisory.

## Coordinated disclosure

We follow a 90-day disclosure window by default, counting from the
acknowledgement date. We will publish a GitHub Security Advisory and credit
reporters who wish to be credited. If a fix is impractical within 90 days we
will request an extension before the window closes.

## Scope

In scope:

- Manifests under `k8s/` and Talos patches under `talos/` and `talos-local/`.
- Workflow definitions under `.github/workflows/` and supporting scripts.
- Documentation that, if wrong, would lead an operator to a weaker
  configuration (notably `docs/dr/`, `docs/secret-rotation.md`,
  `docs/oidc-kubectl.md`).
- The KSail configuration files (`ksail.yaml`, `ksail.prod.yaml`) and the
  Hetzner provisioning script under `hetzner/`.

Out of scope:

- Vulnerabilities in upstream container images, Helm charts, or
  Talos/Kubernetes itself. Please report those to the respective upstream
  projects. We will of course update the affected component once an upstream
  fix is available; tracking issues for that are welcome as normal issues.
- Theoretical findings on encrypted SOPS payloads (`*.enc.yaml`). The
  ciphertext is intentionally checked into git; finding ciphertext is not a
  vulnerability. The corresponding private Age keys are not stored in this
  repository.
- Self-XSS, missing security headers on pages that do not handle authenticated
  state, and other findings without a realistic exploit path.

## Cryptographic material

- All in-repo secrets are SOPS-encrypted with Age (`.sops.yaml` lists the
  current recipients). Reporting that ciphertext is present in git is not a
  vulnerability.
- The platform OCI artifact published to GHCR is signed via cosign keyless
  signing (Fulcio + Rekor) against the GitHub Actions OIDC identity of this
  repository's `cd.yaml` workflow. Tampering would require compromising
  Fulcio, Rekor, or the workflow itself.
- Talos PKI, OpenBao unseal material, and the Hetzner API token are the
  three pieces of cryptographic material whose loss would force a full
  cluster rebuild; their custody is documented in `docs/dr/runbook.md`.

## Public-repository hardening

Because this repository is public, the following constraints apply to all
contributions:

- Plaintext secrets must never be committed. All Kubernetes Secrets and any
  other sensitive material must be SOPS-encrypted (`*.enc.yaml`) and gated
  by the `.sops.yaml` rules.
- Third-party GitHub Actions must be pinned by commit SHA (not by tag).
  Renovate keeps these up to date.
- The platform OCI artifact is signed and verified end-to-end; bypassing
  that verification in a PR is treated as a security regression.

[CVSS v3.1]: https://www.first.org/cvss/v3.1/specification-document
