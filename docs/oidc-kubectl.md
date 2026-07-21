# kubectl OIDC Login via Dex

This guide explains how to use [`kubelogin`](https://github.com/int128/kubelogin) to authenticate `kubectl` against the Kubernetes API via Dex and GitHub.

OIDC is the **default, day-to-day** way to reach the cluster and is intentionally **read-only** (see [RBAC](#rbac)). Write/admin access is **break-glass only** — the root client-certificate kubeconfig kept in the vault (see [Break-glass admin access](#break-glass-admin-access)).

## Prerequisites

- Access to the cluster (kubeconfig with server address)
- A GitHub account that is a member of the [`devantler-tech`](https://github.com/devantler-tech) organization
- Your OIDC identity must be bound to the read-only roles (see [RBAC](#rbac))

## 1 — Install kubelogin

```bash
# Homebrew (macOS / Linux)
brew install int128/kubelogin/kubelogin

# Or with krew
kubectl krew install oidc-login
```

Verify: `kubectl oidc-login --help`

## 2 — Add an OIDC user to kubeconfig

### Local cluster (`platform.lan`)

```bash
kubectl config set-credentials oidc-local \
  --exec-api-version=client.authentication.k8s.io/v1 \
  --exec-command=kubectl \
  --exec-arg=oidc-login \
  --exec-arg=get-token \
  --exec-arg=--oidc-issuer-url=https://dex.platform.lan \
  --exec-arg=--oidc-client-id=kubectl \
  --exec-arg=--oidc-extra-scope=email \
  --exec-arg=--oidc-extra-scope=profile \
  --exec-arg=--oidc-extra-scope=groups \
  --exec-arg=--certificate-authority-data=$(cat "$(mkcert -CAROOT)/rootCA.pem" | base64 | tr -d '\n')
```

> **Note:** The `--certificate-authority-data` flag is only needed for
> local development where TLS is signed by mkcert.  You can find the CA at
> `$(mkcert -CAROOT)/rootCA.pem`.

### Production cluster (`platform.devantler.tech`)

```bash
kubectl config set-credentials oidc-prod \
  --exec-api-version=client.authentication.k8s.io/v1 \
  --exec-command=kubectl \
  --exec-arg=oidc-login \
  --exec-arg=get-token \
  --exec-arg=--oidc-issuer-url=https://dex.platform.devantler.tech \
  --exec-arg=--oidc-client-id=kubectl \
  --exec-arg=--oidc-extra-scope=email \
  --exec-arg=--oidc-extra-scope=profile \
  --exec-arg=--oidc-extra-scope=groups
```

## 3 — Create a context that uses the OIDC user

```bash
# Local
kubectl config set-context oidc@local \
  --cluster=local \
  --user=oidc-local

# Production
kubectl config set-context oidc@prod \
  --cluster=prod \
  --user=oidc-prod
```

## 4 — Test

```bash
kubectl config use-context oidc@local   # or oidc@prod
kubectl get nodes

# Confirm the identity and that access is read-only:
kubectl auth whoami                      # Username: oidc:ned@devantler.tech
kubectl auth can-i list pods             # yes
kubectl auth can-i get secrets           # no  (Secrets are excluded by design)
kubectl auth can-i create deployments    # no  (writes are break-glass only)
```

On the first run, `kubelogin` opens a browser window. Log in with GitHub
through Dex. Once authenticated, the token is cached locally and refreshed
automatically.

## Authentication flow

```
kubectl get pods
      │
      ▼
kubelogin (exec credential plugin)
      │  opens browser → https://dex.{domain}/auth
      │                        │
      │                        ▼
      │                  GitHub OAuth login
      │                        │
      │                        ▼
      │                  Dex issues ID token
      │  ◄─── http://localhost:8000 callback
      │
      ▼
kubectl sends ID token as Bearer header
      │
      ▼
kube-apiserver validates token
  • oidc-issuer-url matches token issuer ✓
  • oidc-client-id matches token audience ✓
  • signature verified against Dex JWKS ✓
  • email claim → Kubernetes username
  • groups claim → Kubernetes groups
      │
      ▼
RBAC: ClusterRoleBindings oidc-view + oidc-cluster-reader
  grant READ-ONLY (view + cluster-reader) to oidc:ned@devantler.tech
  (admin is break-glass via the root cert — not OIDC)
```

## RBAC

OIDC access is **read-only**. Two ClusterRoleBindings in
`k8s/bases/infrastructure/cluster-role-bindings/oidc-view.yaml` and `oidc-cluster-reader.yaml` bind the
user whose email matches the Dex `email` claim (`oidc:${admin_email}`) to:

| Binding | Role | Grants |
|---|---|---|
| `oidc-view` | built-in `view` | read all **namespaced** resources (pods, deployments, configmaps, pod logs, events, …) — **excluding Secrets** |
| `oidc-cluster-reader` | `cluster-reader` | read what `view` omits — mostly **cluster-scoped** infra (nodes, PVs, storage classes, CRDs, API services, priority/runtime/ingress classes, CSRs) plus RBAC objects and node/pod metrics |

```yaml
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: "oidc:${admin_email}"   # → oidc:ned@devantler.tech
```

What is deliberately **not** granted via OIDC: Secrets (kept in the vault on
purpose) and any write/exec verb (`create`/`update`/`delete`, `pods/exec`,
`pods/portforward`). The `cluster-reader` ClusterRole is defined in
`k8s/bases/infrastructure/cluster-roles/cluster-reader.yaml`.

To grant read-only access to additional users, add more subjects to both
bindings (or switch the subject to a Dex group such as
`oidc:devantler-tech:platform`).

## Break-glass admin access

There is **no admin path via OIDC** — by design. When a write or an
otherwise-forbidden operation is genuinely required, use the root
**client-certificate** kubeconfig stored in the vault:

1. Retrieve the root kubeconfig from the vault and point `KUBECONFIG` at it
   (or merge its `admin@prod` context), e.g.:

   ```bash
   export KUBECONFIG=/path/to/root-kubeconfig.yaml
   kubectl config use-context admin@prod
   ```

2. Do the minimal change, then switch back to the OIDC context:

   ```bash
   unset KUBECONFIG                       # back to ~/.kube/config
   kubectl config use-context oidc@prod
   ```

The root cert authenticates directly against the cluster CA and bypasses
OIDC/RBAC role limits (it is `cluster-admin`), so treat it accordingly: pull
it only when needed and never persist it in your day-to-day kubeconfig.

Last-resort regeneration (if the vault copy is lost): a fresh admin
kubeconfig can be minted from the Talos control plane with
`talosctl --talosconfig <admin-talosconfig> kubeconfig`.

## Dex client configuration

`kubelogin` uses the `kubectl` static client defined in the Dex
HelmRelease. This is a **public client** (no secret required) that uses
Dex's [cross-client trust](https://dexidp.io/docs/configuration/custom-scopes-claims-clients/#cross-client-trust-and-authorized-party)
(`trustedPeers`) so the issued token has `aud: public-client`, matching the
kube-apiserver's `--oidc-client-id` flag.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `error: You must be logged in to the server (Unauthorized)` | Token expired or wrong audience | Run `kubectl oidc-login setup --oidc-issuer-url=... --oidc-client-id=kubectl` to verify the flow |
| Browser doesn't open | kubelogin not installed or not in `$PATH` | Verify `kubectl oidc-login --help` works |
| `x509: certificate signed by unknown authority` (local) | mkcert CA not trusted | Pass `--certificate-authority-data` or install the mkcert root CA in your system trust store |
| `Forbidden` on a **read** | Email not bound, or resource outside the read-only roles | Check `kubectl auth whoami`; confirm the email matches the `oidc-view.yaml` / `oidc-cluster-reader.yaml` subjects |
| `Forbidden` on a **write** | Expected — OIDC is read-only | Use the [break-glass admin cert](#break-glass-admin-access) for writes |

## References

- [Dex Kubernetes guide](https://dexidp.io/docs/guides/kubernetes/)
- [kubelogin README](https://github.com/int128/kubelogin)
- [Kubernetes OIDC authentication](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#openid-connect-tokens)
