# Fleet Device Management

Fleet Device Management is a free and open source device management solution
for teams of any size. It supports all major operating systems while
allowing low-level access to OS-specific features through osquery.

- [Documentation](https://fleetdm.com/docs/)
- [Helm Chart](https://github.com/fleetdm/fleet/tree/main/charts/fleet)
- [Chart values reference](https://github.com/fleetdm/fleet/blob/main/charts/fleet/values.yaml)

## Platform integration

- **Ingress**: exposed via a Gateway API `HTTPRoute` attached to the
  `platform/kube-system` Gateway, hostname `fleetdm.${domain}`. TLS
  terminates at the Gateway — Fleet speaks cleartext HTTP in-cluster
  (`fleet.tls.enabled: false`).
- **Reachability**: unlike `homepage`, the Fleet endpoint is **not**
  behind `oauth2-proxy` — enrolled devices cannot complete an OIDC flow.
  Traffic flows Cloudflare Tunnel → Gateway → Fleet Service.
- **HA**:
  - Fleet API: `${fleetdm_replicas:=2}` with a PodDisruptionBudget
    (`minAvailable: 1`), pod anti-affinity (chart default), and
    topology spread across nodes;
  - MySQL: Bitnami subchart in `replication` mode (1 primary + 2
    secondaries, prod) — `standalone` (local);
  - Redis: Bitnami subchart in `replication` mode (2 replicas, prod)
    — `standalone` (local).
- **Vulnerability processing**: runs in a dedicated container
  (`vulnProcessing.dedicated: true`) so the API pods don't OOM every
  hour during the scan.

## Secrets

All secrets live in the per-cluster SOPS-encrypted
`k8s/clusters/<env>/variables/variables-cluster-secret.enc.yaml` and are
substituted at Flux reconcile time:

| Variable | Purpose |
| --- | --- |
| `fleetdm_server_private_key` | **Fleet MDM server private key.** 32 random bytes (hex). **Must be stable forever** — losing it invalidates every enrolled device. |
| `fleetdm_mysql_password` | Fleet DB user password. |
| `fleetdm_mysql_root_password` | MySQL root password. |
| `fleetdm_mysql_replication_password` | MySQL replication user password. |
| `fleetdm_redis_password` | Redis password. |

### Rotating / regenerating

```sh
# Generate a new MDM private key (WARNING: invalidates all enrolled devices)
NEW_KEY=$(openssl rand -hex 32)
sops set k8s/clusters/prod/variables/variables-cluster-secret.enc.yaml \
  '["stringData"]["fleetdm_server_private_key"]' "\"$NEW_KEY\""

# Generate a new password
NEW_PASS=$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-28)
sops set k8s/clusters/prod/variables/variables-cluster-secret.enc.yaml \
  '["stringData"]["fleetdm_mysql_password"]' "\"$NEW_PASS\""
```

Rotating MySQL / Redis passwords also requires updating the Bitnami
subcharts' running instances — the simplest path for this homelab is to
scale the StatefulSet to 0, delete the PVCs, and let Flux recreate
everything with the new credentials.

## Image registry note

The bundled Bitnami MySQL / Redis subcharts pin tags that Broadcom moved
out of `docker.io/bitnami/*` to `docker.io/bitnamilegacy/*` in
[August 2025](https://github.com/bitnami/containers/issues/83267). The
HelmRelease overrides each image's `repository` to `bitnamilegacy/*`
until the Fleet chart ships updated subchart pins or we migrate to the
official `mysql` / `redis` images.
