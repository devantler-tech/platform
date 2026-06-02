# Runtime security

This is the deep dive on **runtime** security: how the platform detects and
stops malicious behaviour *while workloads are running*, as opposed to blocking
it at admission ([Kyverno](../k8s/bases/infrastructure/cluster-policies/)) or
hardening it at the OS layer ([Talos](../talos/)).

It exists mainly to answer one recurring question: **why do we run two
eBPF-based runtime sensors — Kubescape's node-agent *and* Tetragon — instead of
picking one?** The short answer is that they are not the same kind of tool;
the long answer is below.

> Guiding principle: **detection and enforcement are different jobs, and the
> best tool for each is different.** We accept one deliberate overlap (two
> eBPF agents) to get both done well, and we write down the conditions under
> which we'd revisit that.

---

## The runtime stack at a glance

Runtime security here is layered. The two eBPF sensors are the headline, but
they sit inside a wider set of controls:

| Layer | Control | What it does at runtime |
| --- | --- | --- |
| Kernel LSM | **AppArmor** ([`talos/cluster/apparmor.yaml`](../talos/cluster/apparmor.yaml)) | Confines container processes to a profile; default-deny for unexpected file/cap access |
| Syscall filter | **seccomp `RuntimeDefault`** (mutated + enforced by [Kyverno](../k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml)) | Blocks the dangerous-syscall tail every container gets by default |
| Kernel hardening | **sysctls** ([`talos/cluster/sysctls.yaml`](../talos/cluster/sysctls.yaml)) | `kptr_restrict`, `ptrace_scope`, unprivileged-eBPF off, etc. — shrinks the local-privesc surface |
| Network | **Cilium + Hubble** | L3–L7 flow visibility and default-deny [CiliumNetworkPolicy](../k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml) per namespace |
| Runtime detection | **Kubescape node-agent** | Learned-behaviour anomaly detection, correlated with config/CVE/compliance posture |
| Runtime enforcement | **Tetragon** | Declarative kernel-hook policies that can **kill** a process inline |
| Forensics | **API audit log** ([`talos/cluster/audit-logging.yaml`](../talos/cluster/audit-logging.yaml)) | Who-did-what record of control-plane mutations |

This document focuses on the two middle-to-bottom rows — the eBPF sensors.

---

## The two sensors

### Kubescape node-agent — *detection, tied to posture*

Enabled in [`kubescape/helm-release.yaml`](../k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml):

```yaml
capabilities:
  configurationScan: enable   # CIS / NSA / MITRE compliance
  continuousScan: enable
  vulnerabilityScan: enable   # image CVEs (kubevuln)
  runtimeDetection: enable    # ← the eBPF node-agent
hostScanner:
  enabled: true
nodeAgent:
  config:
    hostSensor:
      enabled: true
```

Kubescape is a **posture platform** that happens to ship a runtime sensor. Its
node-agent builds a learned profile of each workload's normal syscall/file/
network behaviour and flags deviations — and, crucially, it reports those
deviations **in the same pane as** the config-scan results, the image-CVE
inventory, and the CIS/NSA/MITRE compliance frameworks. That correlation is the
point: "this container is doing something unusual *and* it has a known-exploitable
CVE *and* it violates control C-00xx" is a far stronger signal than any of those
alone.

What it does **not** do: it is **detection-only**. It will tell you a process
misbehaved; it will not stop it.

### Tetragon — *enforcement and precise kernel observability*

Being introduced via the stacked PRs
[#1688](https://github.com/devantler-tech/platform/pull/1688) (install +
observability), [#1689](https://github.com/devantler-tech/platform/pull/1689)
(observe-only `TracingPolicy` for sensitive files), and
[#1690](https://github.com/devantler-tech/platform/pull/1690) (opt-in
enforcement). Lives under
`k8s/bases/infrastructure/controllers/tetragon/` and
`k8s/bases/infrastructure/tracing-policies/`.

Tetragon is a **purpose-built runtime engine**. Two things make it
complementary rather than redundant:

1. **Enforcement.** A `TracingPolicy` can carry a `Sigkill` action, so Tetragon
   can terminate a process *in-kernel, synchronously*, the moment it crosses a
   line (e.g. writes to a protected system path). Detection tools can only alert
   after the fact. Enforcement is opt-in per workload (via a pod label) so it
   starts safe.
2. **Precise, declarative kernel hooks.** `TracingPolicy` targets specific
   kprobes/tracepoints (a syscall, an LSM hook, a file path) with low overhead,
   rather than profiling everything. That makes it the right tool for narrow,
   high-value invariants like "no one rewrites `/etc` or the kubelet binary."

What it does **not** do: it has no notion of compliance frameworks, image CVEs,
or a learned baseline of "normal" — it enforces and observes the rules *you
write*, nothing more.

---

## Why both, and not one

| If we kept only… | We would lose |
| --- | --- |
| **Tetragon** | Compliance/CVE/posture correlation, learned-behaviour anomaly detection, the single-pane Kubescape view — i.e. *"is this CVE actually reachable at runtime?"* |
| **Kubescape node-agent** | Inline **enforcement** (Sigkill), and expressive low-overhead kernel-hook policies for system-file integrity |

They sit at opposite ends of the detect → enforce spectrum:

```
        Kubescape node-agent                         Tetragon
   ┌────────────────────────────┐        ┌────────────────────────────┐
   │ broad, learned, correlated │        │ narrow, declared, enforcing│
   │ "something looks wrong AND  │        │ "this exact thing is        │
   │  it maps to a known risk"   │   vs.  │  forbidden — kill it now"   │
   │ detection only              │        │ detection + enforcement     │
   └────────────────────────────┘        └────────────────────────────┘
```

Consolidating onto either one would either give up the ability to **block**
attacks (Kubescape-only) or give up **posture-correlated detection**
(Tetragon-only). For a platform that is going public, we want both.

---

## The cost we accept

- **Two sets of eBPF programs** are loaded on every node (each sensor attaches
  its own probes). That is real memory and a little CPU per node. On the
  memory-constrained prod nodes this is bounded but not free — see
  [`docs/node-autoscaling.md`](node-autoscaling.md) and the resource
  right-sizing work for the budget context.
- **Overlapping syscall visibility.** Both sensors can see `exec`/`open` events.
  We accept the duplication because each consumes those events for a different
  purpose (anomaly baseline vs. policy match).

---

## When to revisit this decision

This is a *deliberate* overlap, not a permanent one. Reopen it if either of
these becomes true:

1. **Node memory pressure becomes acute.** The first lever is to narrow the
   **heavier** sensor: set Kubescape `capabilities.runtimeDetection: disable`
   (keeping its config-scan, image-CVE, and compliance scanning, which are its
   real differentiators) and let **Tetragon own all runtime**. This trades
   posture-correlated anomaly detection for a smaller footprint.
2. **Tetragon's detection matures** — if observe-only `TracingPolicy` coverage
   (plus any future anomaly tooling around it) grows to cover the cases we rely
   on Kubescape's node-agent for, consolidating onto Tetragon becomes the
   simpler architecture.

Until one of those holds, **both run, by design.**
