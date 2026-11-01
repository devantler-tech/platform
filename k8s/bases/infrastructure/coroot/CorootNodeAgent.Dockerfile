FROM ghcr.io/coroot/coroot-node-agent:1.38.1@sha256:a1488ca3e541703fe4b4cdefeff2dae992cc9f45affd18f9f41e5b12a8016cae

COPY --chmod=0755 coroot-node-agent /usr/bin/coroot-node-agent
