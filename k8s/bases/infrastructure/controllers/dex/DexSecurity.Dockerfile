# The native executable is compiled, scanned and exercised before this step.
# Preserve SQLite support with a static CGO build; omit the unused gomplate
# launcher. Helm's existing [dex, serve, ...] arguments execute directly.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --chmod=0755 dist/dex /usr/local/bin/dex
COPY web /srv/dex/web
COPY dist/licenses /usr/share/licenses/dex
ENV PATH=/usr/local/bin
USER 65534:65534
ENTRYPOINT []
CMD ["dex", "serve", "/etc/dex/config.yaml"]
