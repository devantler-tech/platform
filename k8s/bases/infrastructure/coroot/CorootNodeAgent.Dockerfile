FROM ghcr.io/coroot/coroot-node-agent:1.35.8@sha256:a08143e4ea42420d4d8b8b2cd21f67dc76743d6e0e37ab9a97b304171e670b93

COPY --chmod=0755 coroot-node-agent /usr/bin/coroot-node-agent
