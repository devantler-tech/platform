FROM ghcr.io/stalwartlabs/cli:1.0.13@sha256:9a1e07307d6f890a193322c4b737fc0868405f0f8486a19922cbf362f43884fd AS cli
FROM caddy:2.11.9@sha256:4851e54a46b206517c574821bb5cc67f170d2156041e9fbc4c9193b45e84a75b AS client-proxy
# This adapter only binds an unprivileged port. The upstream binary's effective
# file capability prevents exec when the container drops every capability.
RUN setcap -r /usr/bin/caddy && test -z "$(getcap /usr/bin/caddy)"
# The spam filter rules release pinned by digest: stalwartlabs/spam-filter
# v3.0.2, built from tag commit 3d2eea7f045ccf9cbb67eea131f627880c7dc983. The
# image keeps only the copy derived from it below.
FROM scratch AS spam-filter-release
ADD --checksum=sha256:1d3a66f6fa9a32f711c5b3a73853663eff27c994a46e8f6a0db64a9b2a580f52 https://github.com/stalwartlabs/spam-filter/releases/download/v3.0.2/spam-filter-rules.json.gz /spam-filter-rules.json.gz
FROM ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c

USER 0
RUN mkdir -p /usr/local/share/stalwart /usr/local/share/licenses/stalwart-webui /usr/local/share/licenses/stalwart-spam-filter && \
    chmod 0755 /usr/local/share /usr/local/share/stalwart /usr/local/share/licenses /usr/local/share/licenses/stalwart-webui /usr/local/share/licenses/stalwart-spam-filter
COPY --from=cli /usr/local/bin/stalwart-cli /usr/local/bin/stalwart-cli
COPY --from=client-proxy --chmod=0755 /usr/bin/caddy /usr/local/bin/caddy
# COPY cannot fetch a URL. ADD fetches these files and BuildKit verifies each pinned digest.
# checkov:skip=CKV_DOCKER_4:Remote fetches of checksum-pinned files, which COPY cannot do.
ADD --chmod=0444 --checksum=sha256:cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30 https://raw.githubusercontent.com/caddyserver/caddy/v2.11.8/LICENSE /usr/local/share/licenses/caddy/LICENSE
ADD --chmod=0444 --checksum=sha256:23f18e03dc49df91622fe2a76176497404e46ced8a715d9d2b67a7446571cca3 https://raw.githubusercontent.com/stalwartlabs/spam-filter/3d2eea7f045ccf9cbb67eea131f627880c7dc983/LICENSE-MIT /usr/local/share/licenses/stalwart-spam-filter/LICENSE-MIT
COPY --chmod=0755 private-mail-bootstrap /usr/local/bin/private-mail-bootstrap
COPY --chmod=0444 webui.zip /usr/local/share/stalwart/webui.zip
COPY --chmod=0444 webui.LICENSE /usr/local/share/licenses/stalwart-webui/LICENSE
COPY --chmod=0444 webui.SOURCE /usr/local/share/licenses/stalwart-webui/SOURCE
RUN asset_checksum="$(sha256sum /usr/local/share/stalwart/webui.zip)" && \
    asset_digest="${asset_checksum%% *}" && \
    mv /usr/local/share/stalwart/webui.zip "/usr/local/share/stalwart/webui-${asset_digest}.zip" && \
    chmod 0755 /usr/local/share/licenses/caddy
# The initializer refuses any other release or object counts and leaves out
# only the shortened-link lookup (docs/operations/email-secure-defaults.md).
RUN --mount=type=bind,from=spam-filter-release,source=/spam-filter-rules.json.gz,target=/tmp/spam-filter-rules.json.gz \
    /usr/local/bin/private-mail-bootstrap spam-rules --input /tmp/spam-filter-rules.json.gz --output /usr/local/share/stalwart/spam-filter-rules-v3.0.2.json

USER 2000:2000
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/private-mail-bootstrap", "bootstrap"]
CMD []
