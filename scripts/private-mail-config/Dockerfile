FROM ghcr.io/stalwartlabs/cli:1.0.13@sha256:9a1e07307d6f890a193322c4b737fc0868405f0f8486a19922cbf362f43884fd AS cli
FROM ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c

COPY --from=cli /usr/local/bin/stalwart-cli /usr/local/bin/stalwart-cli
COPY --chmod=0755 private-mail-bootstrap /usr/local/bin/private-mail-bootstrap

USER 2000:2000
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/private-mail-bootstrap", "bootstrap"]
CMD []
