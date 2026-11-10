FROM docker.io/longhornio/longhorn-manager:v1.13.0@sha256:dfe39d0ce270c68b333d8b32bec41ee540b7a099ccc598ea312337a85c20fd89
COPY --chmod=0755 longhorn-manager /usr/local/sbin/longhorn-manager
