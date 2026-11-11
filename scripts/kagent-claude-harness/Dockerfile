# kagent's Claude Code harness, as the kagent 1.0.0 release pins it, plus
# what the Agentic Engineer's runs use (devantler-tech/monorepo#4867, #4868):
#
# - gh for the GitHub API, jq and yq for JSON and YAML, and curl.
# - The whole agentic-engineering plugin at a pinned agent-plugins commit,
#   loaded by a `claude` wrapper until kagent loads it whole itself (claude).
#
# The harness binary, Claude Code and its pinned version are the base image's,
# unchanged.

# The plugin, fetched by its exact commit. Renovate moves the commit to the
# head of agent-plugins' main (.github/renovate.json), which only reviewed pull
# requests reach.
FROM ghcr.io/kagent-dev/kagent/claude-harness:1.0.0@sha256:54629b4e47ad08ab931ae815492d2533f4a3dd6ce007eda06863675afb4f99d2 AS plugin
ARG AGENT_PLUGINS_COMMIT=eb6d31f70c8cfa95fe0292878c451e2e90a11b5f
# The base image's unprivileged user fetches into /tmp, which it can write.
RUN git init -q /tmp/src && \
    git -C /tmp/src fetch -q --depth 1 https://github.com/devantler-tech/agent-plugins.git "${AGENT_PLUGINS_COMMIT}" && \
    git -C /tmp/src checkout -q FETCH_HEAD && \
    test "$(git -C /tmp/src rev-parse HEAD)" = "${AGENT_PLUGINS_COMMIT}" && \
    test -f /tmp/src/plugins/agentic-engineering/.claude-plugin/plugin.json && \
    test -d /tmp/src/plugins/agentic-engineering/agents && \
    test -d /tmp/src/plugins/agentic-engineering/skills

FROM ghcr.io/kagent-dev/kagent/claude-harness:1.0.0@sha256:54629b4e47ad08ab931ae815492d2533f4a3dd6ce007eda06863675afb4f99d2

USER 0
# Alpine's package index moves with the base image's release branch, and the
# published image is pinned by digest wherever it runs, so versions are not
# pinned here.
# hadolint ignore=DL3018
RUN apk add --no-cache curl github-cli jq yq-go && \
    gh --version && jq --version && yq --version && curl --version

COPY --from=plugin /tmp/src/plugins/agentic-engineering /opt/agent-plugins/agentic-engineering
# The wrapper takes Claude Code's place on PATH, where the harness looks for it.
RUN mkdir -p /usr/local/libexec/claude-code && \
    mv /usr/local/bin/claude /usr/local/libexec/claude-code/claude
COPY --chmod=0755 claude /usr/local/bin/claude
RUN claude --version && \
    test -f /opt/agent-plugins/agentic-engineering/.claude-plugin/plugin.json

# The base image's kagent user, home and entrypoint.
USER 65532:65532
HEALTHCHECK NONE
