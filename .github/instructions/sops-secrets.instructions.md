---
description: "Use when working with SOPS-encrypted files (*.enc.yaml), Age encryption keys, or Kubernetes secrets in the platform. Covers encryption/decryption workflow and per-environment key rules."
applyTo: "**/*.enc.yaml"
---
# SOPS Secret Management

## Encryption Rules (`.sops.yaml`)

Each environment has its own Age public key. Path-based rules auto-select the key:
- `k8s/clusters/local/*.enc.yaml` → local Age key
- `k8s/clusters/dev/*.enc.yaml` → dev Age key
- `k8s/clusters/prod/*.enc.yaml` → prod Age key

Only `data` and `stringData` fields are encrypted (`encrypted_regex: ^(data|stringData)$`).

## Working with Secrets

```bash
# Decrypt (requires matching Age private key)
sops -d k8s/clusters/local/variables/variables-cluster-secret.enc.yaml

# Encrypt a new secret (SOPS auto-selects key from .sops.yaml path rules)
sops -e secret.yaml > secret.enc.yaml

# Edit in-place
sops k8s/clusters/local/variables/variables-cluster-secret.enc.yaml
```

## Key Requirements

- Local dev key: `~/Library/Application Support/sops/age/keys.txt` (macOS) or `~/.config/sops/age/keys.txt` (Linux)
- CI uses `SOPS_AGE_KEY` environment variable (GitHub Actions secret + Dependabot secret)
- You **cannot decrypt** secrets without the matching private key — fork and re-encrypt with your own key if needed

## Convention

- Secret files **must** use the `.enc.yaml` suffix
- Place secrets under `k8s/clusters/<env>/variables/`
- Never commit unencrypted secrets
