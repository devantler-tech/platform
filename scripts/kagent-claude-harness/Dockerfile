# kagent's Claude Code harness, as the kagent 1.0.0-rc3 release pins it, plus
# the command-line tools the Agentic Engineer's runs use: gh for the GitHub
# API, jq and yq for JSON and YAML, and curl. The harness binary, Claude Code
# and its pinned version are the base image's, unchanged
# (devantler-tech/monorepo#4867).
FROM ghcr.io/kagent-dev/kagent/claude-harness@sha256:216613bf155d3d437765675144fee19c9045cb21cd2e481189f2dcae36c24b3b

USER 0
# Alpine's package index moves with the base image's release branch, and the
# published image is pinned by digest wherever it runs, so versions are not
# pinned here.
# hadolint ignore=DL3018
RUN apk add --no-cache curl github-cli jq yq-go && \
    gh --version && jq --version && yq --version && curl --version

# The base image's kagent user, home and entrypoint.
USER 65532:65532
HEALTHCHECK NONE
