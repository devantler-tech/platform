# Tetragon

Tetragon is Cilium's eBPF-based security observability and runtime enforcement agent. It observes process execution, syscalls, file access, and network activity at the kernel level and exports the events for auditing and alerting.

This install is observability-only: the agent emits its built-in process lifecycle events (plus any `TracingPolicy` added later), exports them to stdout where Alloy collects them into Loki, and exposes Prometheus metrics. Runtime enforcement is not enabled.

- [Documentation](https://tetragon.io/docs/)
- [Helm Chart](https://github.com/cilium/tetragon/tree/main/install/kubernetes/tetragon)
