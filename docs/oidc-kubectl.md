# kubectl OIDC Login via Dex

This guide explains how to use [`kubelogin`](https://github.com/int128/kubelogin) to authenticate `kubectl` against the Kubernetes API via Dex and GitHub.

## Prerequisites

- Access to the cluster (kubeconfig with server address)
- A GitHub account that is a member of the [`devantler-tech`](https://github.com/devantler-tech) organisation
- The `oidc-admin` ClusterRoleBinding must list your OIDC identity (see [RBAC](#rbac))

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
  --exec-api-version=client.authentication.k8s.io/v1beta1 \
  --exec-command=kubectl \
  --exec-arg=oidc-login \
  --exec-arg=get-token \
  --exec-arg=--oidc-issuer-url=https://dex.platform.lan \
  --exec-arg=--oidc-client-id=kubectl \
  --exec-arg=--oidc-extra-scope=email \
  --exec-arg=--oidc-extra-scope=profile \
  --exec-arg=--oidc-extra-scope=groups \
  --exec-arg=--certificate-authority-data=$(cat /path/to/mkcert-ca.pem | base64 | tr -d '\n')
```

> **Note:** The `--certificate-authority-data` flag is only needed for
> local development where TLS is signed by mkcert.  You can find the CA at
> `$(mkcert -CAROOT)/rootCA.pem`.

### Production cluster (`platform.devantler.tech`)

```bash
kubectl config set-credentials oidc-prod \
  --exec-api-version=client.authentication.k8s.io/v1beta1 \
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
RBAC: ClusterRoleBinding oidc-admin
  grants cluster-admin to ned@devantler.tech
```

## RBAC

The `oidc-admin` ClusterRoleBinding
(`k8s/bases/infrastructure/cluster-role-bindings/oidc-admin.yaml`) grants
`cluster-admin` to the user whose email matches the `email` claim returned
by Dex:

```yaml
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: ned@devantler.tech
```

To grant access to additional users, add more subjects to that file.

## Dex client configuration

`kubelogin` uses the `kubectl` static client defined in the Dex
HelmRelease. This is a **public client** (no secret required) that uses
Dex's [cross-client trust](https://dexidp.io/docs/custom-scopes-claims-clients/#cross-client-trust-and-authorized-party)
(`trustedPeers`) so the issued token has `aud: public-client`, matching the
kube-apiserver's `--oidc-client-id` flag.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `error: You must be logged in to the server (Unauthorized)` | Token expired or wrong audience | Run `kubectl oidc-login setup --oidc-issuer-url=... --oidc-client-id=kubectl` to verify the flow |
| Browser doesn't open | kubelogin not installed or not in `$PATH` | Verify `kubectl oidc-login --help` works |
| `x509: certificate signed by unknown authority` (local) | mkcert CA not trusted | Pass `--certificate-authority-data` or install the mkcert root CA in your system trust store |
| `Forbidden` after successful login | Email doesn't match `oidc-admin` subject | Check `kubectl auth whoami` and update the ClusterRoleBinding |

## References

- [Dex Kubernetes guide](https://dexidp.io/docs/guides/kubernetes/)
- [kubelogin README](https://github.com/int128/kubelogin)
- [Kubernetes OIDC authentication](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#openid-connect-tokens)
