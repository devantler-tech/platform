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
| Kernel LSM | **AppArmor** ([`talos/cluster/enable-apparmor.yaml`](../talos/cluster/enable-apparmor.yaml)) | Confines container processes to a profile; default-deny for unexpected file/cap access |
| Syscall filter | **seccomp `RuntimeDefault`** (mutated + enforced by [Kyverno](../k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml)) | Blocks the dangerous-syscall tail every container gets by default |
| Kernel hardening | **sysctls** ([`talos/cluster/harden-kernel-sysctls.yaml`](../talos/cluster/harden-kernel-sysctls.yaml)) | `kptr_restrict`, `ptrace_scope`, unprivileged-eBPF off, etc. — shrinks the local-privesc surface |
| Network | **Cilium + Hubble** | L3–L7 flow visibility and default-deny [CiliumNetworkPolicy](../k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml) per namespace |
| Runtime detection | **Kubescape node-agent** | Learned-behaviour anomaly detection, correlated with config/CVE/compliance posture |
| Runtime enforcement | **Tetragon** | Declarative kernel-hook policies that **terminate the offending process** (SIGKILL) on a policy match |
| Forensics | **API audit log** ([`talos/cluster/enable-audit-logging.yaml`](../talos/cluster/enable-audit-logging.yaml)) | Who-did-what record of control-plane mutations |

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

Installed via [#1688](https://github.com/devantler-tech/platform/pull/1688)
(agent + operator), with its enforcement policy under
`k8s/bases/infrastructure/tracing-policies/`.

Tetragon's job here is the one thing Kubescape's node-agent cannot do:
**enforcement**. We deliberately do **not** use Tetragon to duplicate
Kubescape's runtime *detection* (anomaly profiling, sensitive-file/exec/network
visibility, posture correlation) — that is Kubescape's domain and it does it
better. Tetragon adds:

1. **Enforcement.** A `TracingPolicy` can carry a `SIGKILL` action that
   **terminates the offending process** as soon as Tetragon observes a matching
   event (e.g. a write to a protected system path). This is *post-event*
   termination — the triggering syscall may already have completed, so it kills
   the **process**, it does not block the call. (Tetragon's `Override` action can
   block the syscall itself where the kernel supports it; this platform uses
   SIGKILL.) It does something a detection-only tool can't: it stops the
   workload rather than just alerting. The policy
   ([`enforce-protect-system-files.yaml`](../k8s/bases/infrastructure/tracing-policies/enforce-protect-system-files.yaml))
   is **opt-in per workload** via a pod label, so it starts safe — zero blast
   radius until a workload opts in.
2. **Precise, declarative kernel hooks.** `TracingPolicy` targets specific
   kprobes/tracepoints (a syscall, an LSM hook, a file path) with low overhead.
   That makes it the right tool for narrow, high-value *invariants you enforce*,
   like "no one rewrites `/etc` or a system binary."

What it does **not** do: no compliance frameworks, image CVEs, or a learned
"normal" baseline — and, **by choice, no standalone detection policies** (that
would duplicate Kubescape). It enforces the rules *you write*.

---

## Why both, and not one

| If we kept only… | We would lose |
| --- | --- |
| **Tetragon** | Compliance/CVE/posture correlation, learned-behaviour anomaly detection, the single-pane Kubescape view — i.e. *"is this CVE actually reachable at runtime?"* |
| **Kubescape node-agent** | **Enforcement** — killing an offending process (SIGKILL) on a match (with `Override` available for true syscall-blocking) — and expressive low-overhead kernel-hook policies for system-file integrity |

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

Running two eBPF sensors is a *deliberate* overlap — **Kubescape for detection,
Tetragon for the enforcement Kubescape lacks** — not a permanent one. Reopen it
if either becomes true:

1. **Kubescape gains comparable enforcement.** Tetragon exists here specifically
   because Kubescape's node-agent is detection-only. If Kubescape (or whatever
   posture platform we run) ships process-killing / syscall-blocking enforcement
   we trust, Tetragon's reason to exist shrinks to its precise kernel-hook
   policies — at which point dropping Tetragon and letting Kubescape own all
   runtime becomes the simpler architecture.
2. **Node memory pressure becomes acute.** Two sensors cost memory per node. The
   preferred lever is to trim **Tetragon** to only the enforcement policies in
   active use — *not* to disable Kubescape's runtime detection, which is the
   capability we lean on most (use Kubescape as much as possible).

Until one of those holds, **both run: Kubescape detects, Tetragon enforces.**
