FROM ghcr.io/coroot/coroot-node-agent:1.38.1@sha256:a1488ca3e541703fe4b4cdefeff2dae992cc9f45affd18f9f41e5b12a8016cae

COPY --chmod=0755 coroot-node-agent /usr/bin/coroot-node-agent
# The node agent loads eBPF programs and reads every host process, so it keeps the upstream image's
# root identity (its config User is empty) instead of naming a non-root USER here.
# checkov:skip=CKV_DOCKER_3:The eBPF node agent keeps the upstream image's root identity by design.
# Kubernetes never runs an image health check, so this image declares none.
HEALTHCHECK NONE
