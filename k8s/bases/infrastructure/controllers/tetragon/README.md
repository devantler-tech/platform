# Tetragon

Tetragon is Cilium's eBPF-based security observability and runtime enforcement agent. It observes process execution, syscalls, file access, and network activity at the kernel level and exports the events for auditing and alerting.

On this platform the agent emits its built-in process lifecycle events to stdout, where Coroot's node agent ingests them, and exposes Prometheus metrics.

Enforcement is opt-in per workload. The one policy, [`enforce-protect-system-files`](../../tracing-policies/enforce-protect-system-files.yaml), kills a process that writes to system or binary directories, but only in pods labelled `tetragon.devantler.tech/enforce-file-integrity: "true"`. No workload in this repository carries that label, so nothing is enforced today. Runtime detection is Kubescape's job; see [runtime security](../../../../../docs/runtime-security.md).

- [Documentation](https://tetragon.io/docs/)
- [Helm Chart](https://github.com/cilium/tetragon/tree/main/install/kubernetes/tetragon)
