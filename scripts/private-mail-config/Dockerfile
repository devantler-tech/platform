FROM ghcr.io/stalwartlabs/cli:1.0.13@sha256:9a1e07307d6f890a193322c4b737fc0868405f0f8486a19922cbf362f43884fd AS cli
FROM caddy:2.11.7@sha256:5e5286cb37d44086d651a5363f05f43a692ce4d649d88fb1bd844579f45fa9e8 AS client-proxy
FROM ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c

USER root
RUN mkdir -p /usr/local/share/stalwart /usr/local/share/licenses/stalwart-webui && \
    chmod 0755 /usr/local/share /usr/local/share/stalwart /usr/local/share/licenses /usr/local/share/licenses/stalwart-webui
COPY --from=cli /usr/local/bin/stalwart-cli /usr/local/bin/stalwart-cli
COPY --from=client-proxy --chmod=0755 /usr/bin/caddy /usr/local/bin/caddy
ADD --chmod=0444 --checksum=sha256:cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30 https://raw.githubusercontent.com/caddyserver/caddy/v2.11.7/LICENSE /usr/local/share/licenses/caddy/LICENSE
COPY --chmod=0755 private-mail-bootstrap /usr/local/bin/private-mail-bootstrap
COPY --chmod=0444 webui.zip /usr/local/share/stalwart/webui.zip
COPY --chmod=0444 webui.LICENSE /usr/local/share/licenses/stalwart-webui/LICENSE
COPY --chmod=0444 webui.SOURCE /usr/local/share/licenses/stalwart-webui/SOURCE
RUN asset_checksum="$(sha256sum /usr/local/share/stalwart/webui.zip)" && \
    asset_digest="${asset_checksum%% *}" && \
    mv /usr/local/share/stalwart/webui.zip "/usr/local/share/stalwart/webui-${asset_digest}.zip"

USER 2000:2000
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/private-mail-bootstrap", "bootstrap"]
CMD []
