FROM docker.io/longhornio/longhorn-manager:v1.13.0@sha256:dfe39d0ce270c68b333d8b32bec41ee540b7a099ccc598ea312337a85c20fd89
COPY --chmod=0755 longhorn-manager /usr/local/sbin/longhorn-manager

USER 65532:65532
# Check the manager admission listener. Kubernetes keeps the chart's HTTPS readiness probe.
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s \
    CMD ["/bin/bash", "-ec", "exec 3<>/dev/tcp/127.0.0.1/9502"]
