FROM ghcr.io/coroot/coroot-node-agent:1.38.1@sha256:a1488ca3e541703fe4b4cdefeff2dae992cc9f45affd18f9f41e5b12a8016cae
# checkov:skip=CKV_DOCKER_3:[runtime-premise] the pinned eBPF agent requires root/CAP_SYS_ADMIN; the operator generates privileged hostPID execution and the native premise guard binds this exact binary-only image

COPY --chmod=0755 coroot-node-agent /usr/bin/coroot-node-agent
