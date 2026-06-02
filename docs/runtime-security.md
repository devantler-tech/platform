# Runtime security

This is the deep dive on **runtime** security: how the platform detects and
stops malicious behaviour *while workloads are running*, as opposed to blocking
it at admission ([Kyverno](../k8s/bases/infrastructure/cluster-policies/)) or
hardening it at the OS layer ([Talos](../talos/)).

The platform runs **one** eBPF runtime engine — **Tetragon** — for both
detection and enforcement, and keeps **Kubescape** as a **posture platform**
(config / CVE / compliance scanning) with its node-agent runtime sensor
**disabled**. This document explains that split: what each tool owns, how
Tetragon covers the runtime threats Kubescape's node-agent used to flag, and
what we consciously traded away by consolidating onto one runtime sensor.

> Guiding principle: **detection and enforcement are different jobs, but one
> well-targeted engine can do both.** We previously ran two eBPF sensors and
> documented the conditions under which we'd narrow to one — this is that
> change.

---

## The runtime stack at a glance

Runtime security here is layered. Tetragon is the headline runtime engine, but
it sits inside a wider set of controls:

| Layer | Control | What it does at runtime |
| --- | --- | --- |
| Kernel LSM | **AppArmor** ([`talos/cluster/apparmor.yaml`](../talos/cluster/apparmor.yaml)) | Confines container processes to a profile; default-deny for unexpected file/cap access |
| Syscall filter | **seccomp `RuntimeDefault`** (mutated + enforced by [Kyverno](../k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml)) | Blocks the dangerous-syscall tail every container gets by default |
| Kernel hardening | **sysctls** ([`talos/cluster/sysctls.yaml`](../talos/cluster/sysctls.yaml)) | `kptr_restrict`, `ptrace_scope`, unprivileged-eBPF off, etc. — shrinks the local-privesc surface |
| Network | **Cilium + Hubble** | L3–L7 flow visibility and default-deny [CiliumNetworkPolicy](../k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml) per namespace |
| Runtime detection **+ enforcement** | **Tetragon** | Declarative kernel-hook `TracingPolicy` resources: observe threat behaviours (→ Loki) and optionally **terminate the offending process** (SIGKILL) on a match |
| Posture | **Kubescape** | Config scan (CIS/NSA/MITRE), image-CVE inventory, compliance — **runtime node-agent disabled** |
| Forensics | **API audit log** ([`talos/cluster/audit-logging.yaml`](../talos/cluster/audit-logging.yaml)) | Who-did-what record of control-plane mutations |

This document focuses on the Tetragon row and the Kubescape posture row.

---

## The two tools

### Tetragon — *the runtime engine (detection + enforcement)*

Installed under `k8s/bases/infrastructure/controllers/tetragon/`, with policies
under `k8s/bases/infrastructure/tracing-policies/`. Delivered across
[#1688](https://github.com/devantler-tech/platform/pull/1688) (install +
observability), [#1689](https://github.com/devantler-tech/platform/pull/1689)
(observe-only `TracingPolicy` set), and
[#1690](https://github.com/devantler-tech/platform/pull/1690) (opt-in
enforcement).

Two properties make it a good single runtime engine:

1. **Precise, declarative kernel hooks.** A `TracingPolicy` targets specific
   kprobes/tracepoints/syscalls (an LSM hook, a file path, `finit_module`,
   `ptrace`) with low overhead, rather than profiling everything. That keeps the
   event volume into Loki proportional to *threat-relevant* activity instead of
   all activity.
2. **Enforcement.** A `TracingPolicy` can carry a `SIGKILL` action that
   **terminates the offending process** as soon as Tetragon observes a match.
   This is *post-event* termination — the triggering syscall may already have
   completed, so it kills the **process**, it does not block the call.
   (Tetragon's `Override` action can block the syscall itself where the kernel
   supports it; this platform uses SIGKILL — see #1690.) It is opt-in per
   workload via a pod label, so it starts safe.

Tetragon events are exported to stdout and shipped to Loki by Alloy, and its
metrics are scraped by Prometheus.

What it does **not** do: it has no notion of compliance frameworks, image CVEs,
or a *learned* baseline of "normal" — it observes and enforces the rules *you
write*, nothing more. That gap is the trade we accept (below).

### Kubescape — *posture, runtime sensor off*

Configured in [`kubescape/helm-release.yaml`](../k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml):

```yaml
capabilities:
  configurationScan: enable   # CIS / NSA / MITRE compliance
  continuousScan: enable
  vulnerabilityScan: enable   # image CVEs (kubevuln)
  runtimeDetection: disable   # ← node-agent runtime sensor OFF (Tetragon owns runtime)
hostScanner:
  enabled: true               # host CIS checks — posture, not runtime
```

Kubescape remains the **posture platform**: it scans cluster and host
configuration against compliance frameworks and inventories image CVEs. What it
no longer does here is **runtime threat detection** — its node-agent built a
learned per-workload behaviour profile and flagged deviations, correlated with
the config/CVE/compliance view. That capability is now off; see the trade below.

---

## Why one runtime engine, and how parity is kept

Running two eBPF sensors meant two sets of probes on every node (real memory on
the memory-constrained prod nodes) and overlapping syscall visibility consumed
for two different purposes. Consolidating onto Tetragon removes the second
sensor. The risk in doing so is **losing detection coverage**, so each concrete
threat behaviour Kubescape's node-agent flagged is now covered explicitly:

| Runtime threat behaviour | Now covered by |
| --- | --- |
| Sensitive-file read / system-dir write | Tetragon [`monitor-sensitive-file-access`](../k8s/bases/infrastructure/tracing-policies/monitor-sensitive-file-access.yaml) (#1689) |
| Process execution visibility | Tetragon built-in `process_exec` / `process_exit` (#1688) |
| Kernel module load / unload (rootkits) | Tetragon [`monitor-privileged-operations`](../k8s/bases/infrastructure/tracing-policies/monitor-privileged-operations.yaml) (#1689) |
| Process injection (`ptrace`) | Tetragon `monitor-privileged-operations` (#1689) |
| Privilege / identity transitions (`setuid`/`setgid`) | Tetragon `monitor-privileged-operations` (#1689) |
| System-file tampering — **active blocking** | Tetragon [`enforce-protect-system-files`](../k8s/bases/infrastructure/tracing-policies/enforce-protect-system-files.yaml) (opt-in SIGKILL, #1690) |
| Network flows / egress | **Cilium + Hubble** (already a separate layer — never was Kubescape's job here) |

Net result: detection coverage of concrete behaviours is **maintained or
improved** (enforcement is new), on a single runtime engine.

### What we traded away (be honest about it)

Consolidation is not free. These Kubescape node-agent capabilities are **not**
replicated by Tetragon and are gone until/unless we re-enable it:

- **Learned-behaviour anomaly detection.** Tetragon matches rules you write; it
  has no baseline of "normal" per workload, so genuinely novel/unknown bad
  behaviour that doesn't trip an explicit policy won't be flagged.
- **Posture-correlated runtime alerts.** Kubescape's single-pane *"this
  container is misbehaving AND has an exploitable CVE AND violates control
  C-00xx"* correlation no longer exists — Kubescape still scans config/CVE/
  compliance, but no longer joins that to live runtime behaviour.
- **Malware signature matching** (the node-agent's malware capability).

These are deliberate trade-offs for a single runtime engine plus enforcement,
on memory-constrained nodes. Mitigations: the explicit `TracingPolicy` set
above covers the high-value behaviours, Cilium/Hubble covers network, and the
policy set can grow.

---

## When to revisit this decision

Re-enable Kubescape `capabilities.runtimeDetection` (accepting the second eBPF
sensor and its memory cost) if either becomes true:

1. **Learned-behaviour / posture-correlated detection becomes worth the cost** —
   e.g. an incident slips past the explicit `TracingPolicy` set that an anomaly
   baseline would have caught, and prod node memory has headroom for a second
   sensor.
2. **The explicit policy set proves too coarse** and maintaining parity by hand
   (one policy per threat) becomes more work than running the learned sensor.

Until then, **Tetragon owns runtime; Kubescape owns posture.**
