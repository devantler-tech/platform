FROM ghcr.io/kagent-dev/kagent/controller@sha256:a85cc7452b403e879f592a094dfb3fb3f40b3552a2c1c91956184c7280850ed5

COPY --chmod=0755 app /app
COPY --chmod=0644 LICENSE IMAGE-NOTICE /usr/share/licenses/kagent/

USER 65532:65532

# Match the controller chart's startup and readiness endpoint.
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s \
  CMD ["wget", "-q", "-T", "2", "-O", "/dev/null", "http://127.0.0.1:8083/health"]
