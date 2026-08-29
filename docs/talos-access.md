# Talos API access

Talos API access is certificate-based and role-scoped. Daily operator access
uses Talos's built-in `os:reader` role; `os:admin` is break-glass only.

`os:reader` exposes the safe inspection methods needed for routine diagnosis.
It can list filesystem paths, but cannot read file contents or invoke mutation
methods. Kubernetes access is a separate identity boundary; see
[kubectl OIDC login](./oidc-kubectl.md).

## Reader configuration

Generate the reader certificate from a valid admin configuration on the trusted
operator device. Target a control-plane node explicitly and give the reader a
bounded lifetime:

```bash
talosctl \
  --talosconfig /private/path/to/admin-talosconfig.yaml \
  --context prod \
  --nodes <control-plane-address> \
  config new /private/path/to/prod-reader.yaml \
  --roles=os:reader \
  --crt-ttl=8760h
```

Install the generated reader configuration as `~/.talos/config`; do not merge
it with a file that contains an admin context. The ambient configuration must
contain only `os:reader` contexts, and the active context must report
`Roles: os:reader` under `talosctl config info`.

## Break-glass administration

Keep the root Talos configuration in two trusted recovery stores:

- a locked iCloud Note available from the trusted operator device; and
- the approved OpenBao break-glass path, reachable through the administrator's
  Dex OIDC identity.

Do not merge the root context into `~/.talos/config`. For an exceptional node
operation, retrieve it into a short-lived mode-`0600` file, pass that path with
`--talosconfig`, perform the minimal operation, and remove the file. Confirm
afterward that `talosctl config info` again reports `os:reader`.

If no admin Talos configuration remains, recovery is a cluster-PKI disaster
recovery event; follow [Cryptographic custody](./dr/crypto-custody.md).
