FROM docker.io/longhornio/longhorn-instance-manager:v1.13.0@sha256:399e06a93e3cf6754aab6642fe5a2afa2e1b10bb7975a0e4a412c6e36085bc6e
COPY --chmod=0755 longhorn-instance-manager /usr/local/bin/longhorn-instance-manager
ENV LONGHORN_PLATFORM_V1_RETIREMENT=1
# The managed Kubernetes Pod must declare its storage runtime identity explicitly.
USER 65532:65532
HEALTHCHECK --interval=15s --timeout=10s --start-period=15s CMD ["/usr/local/bin/instance-manager-liveness-probe", "--timeout", "8"]
