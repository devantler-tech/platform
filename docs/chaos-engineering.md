# Chaos engineering

The platform ships a **Chaos Mesh** fault-injection engine for *game days* — the
deliberate, observed injection of failures (pod kills, network faults, IO/stress,
time skew, node-pressure) to prove the platform's resilience controls actually
work: PodDisruptionBudgets, `topologySpreadConstraints`, self-healing, and the
node-lifecycle / Longhorn-volume behaviour that has driven the recurring prod
incidents (#1816, #1820, #2024).

It is the active counterpart to the [disaster-recovery runbooks](dr/runbook.md)
and the [restore drill](dr/restore-drill.md): the runbooks prove we can *recover*;
chaos game days prove we *degrade gracefully* in the first place.

## Why it is opt-in (not always-on)

`chaos-daemon` is a permanently-privileged DaemonSet — it mounts the host
containerd socket and runs with `hostPID` so it can enter a target pod's
namespaces. Leaving that on every node 24/7 is real attack surface for a
capability used a few times a quarter, so Chaos Mesh is **not** in the always-on
base controller aggregate. It follows the kubevirt/cdi precedent: a single-line
opt-in in the provider overlay, enabled for the duration of a game day and
removed afterwards.

## Running a game day

1. **Enable the engine** — uncomment `chaos-mesh` in the target overlay:
   - Local/CI: `k8s/providers/docker/infrastructure/controllers/kustomization.yaml`
   - Prod: `k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml`

   Then `ksail workload push && ksail workload reconcile` (local) or merge the PR
   (prod). The namespace is PSA `privileged` and pre-exempted in the Kyverno
   cluster-policies, so the daemon admits.

2. **Pick a steady-state hypothesis** — e.g. *"killing one `whoami` replica never
   drops the route, because the PDB + 2 replicas + spread keep one serving."*
   Watch it in Coroot (`observability.${domain}`) — request success-rate and
   latency are the SLIs that must hold.

3. **Apply a scoped experiment** (examples below). Start small: one pod, one
   namespace, a short `duration`.

4. **Observe** the SLO in Coroot for the experiment window; confirm the
   hypothesis held (or file the gap it exposed).

5. **Clean up** — delete the experiment (`kubectl delete -f <experiment>.yaml`),
   re-comment `chaos-mesh` in the overlay, and reconcile. Chaos Mesh also
   auto-recovers a fault when its `duration` elapses or the CR is deleted.

## Safety rules

- **Never target the platform's own foundations.** Scope every experiment's
  `selector.namespaces` to a *non-critical app* (`whoami`, `homepage`). Never
  target `kube-system`, `flux-system`, `longhorn-system`, `chaos-mesh`,
  `observability`, `openbao`, or `cnpg-*` — a fault there can wedge the cluster
  or the very tooling you need to recover.
- **Always set a `duration`** so a fault self-heals even if you lose your
  session.
- **One variable at a time**, and only when you are watching.
- **Prod game days run in a maintenance window**, announced via the same Slack
  channel the Coroot alerts post to.

## Example experiments

Experiments are Git-defined CRs reviewed through a PR — they are intentionally
*not* committed into the always-on kustomizations. Copy one, scope it, apply it.

### Pod kill — validate self-healing + PDB

Proves a single-replica loss is absorbed without dropping the service.

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: whoami-pod-kill
  namespace: chaos-mesh
spec:
  action: pod-kill
  mode: one # kill exactly one matching pod
  selector:
    namespaces:
      - whoami
    labelSelectors:
      app.kubernetes.io/name: whoami
  duration: 30s
```

### Scheduled pod failure — recurring resilience check

A `Schedule` makes a fault recurring (e.g. a weekly game day). Keep it scoped and
short; remove it when the game day ends.

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: Schedule
metadata:
  name: weekly-homepage-pod-failure
  namespace: chaos-mesh
spec:
  schedule: "0 9 * * 1" # Mondays 09:00 — inside a maintenance window
  type: PodChaos
  historyLimit: 5
  concurrencyPolicy: Forbid
  podChaos:
    action: pod-failure # make the pod unavailable (not deleted) for the window
    mode: one
    selector:
      namespaces:
        - homepage
    duration: 60s
```

### Network latency — validate timeout/retry budgets

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: whoami-latency
  namespace: chaos-mesh
spec:
  action: delay
  mode: one
  selector:
    namespaces:
      - whoami
  delay:
    latency: "200ms"
    jitter: "50ms"
  duration: 60s
```

### Node-lifecycle rehearsal (the recurring prod incident class)

The node-roll / autoscaler-churn faults that strand Longhorn volumes (#1816,
#1820, #2024) are best rehearsed by draining a *storage* worker and watching
Longhorn re-attach. Chaos Mesh's `StressChaos` (CPU/memory pressure) can
reproduce the eviction pressure that triggers it; pair it with a manual
`kubectl drain` of one Longhorn worker and confirm volumes re-attach and the
SPIRE/ExternalSecrets chain stays healthy. Document findings on the relevant
Theme-1 issue.

## Reference

- Chaos Mesh docs: <https://chaos-mesh.org/docs/>
- Experiment types: PodChaos, NetworkChaos, StressChaos, IOChaos, TimeChaos,
  DNSChaos (the DNSChaos server is left off by default — enable `dnsServer` in
  the HelmRelease if a game day needs it).
