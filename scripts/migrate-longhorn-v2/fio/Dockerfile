FROM docker.io/library/debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392 AS build
SHELL ["/bin/bash", "-o", "pipefail", "-euxc"]
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc=4:14.2.0-1 libc6-dev=2.41-12+deb13u5 libaio-dev=0.3.113-8+b1 make=4.4.1-2 pkgconf=1.8.1-4 && \
    rm -rf /var/lib/apt/lists/*
COPY fio.tar.gz /tmp/fio.tar.gz
WORKDIR /tmp/fio
RUN printf '%s  %s\n' '5cacff5a41c4ad89c66f0348676156d56b680a3871c8d2da29a02d10cdef09ab' /tmp/fio.tar.gz | sha256sum --check && \
    tar --strip-components=1 -xzf /tmp/fio.tar.gz && \
    ./configure --disable-native --extra-cflags='-march=x86-64 -mtune=generic' && \
    grep -qx 'CONFIG_LIBAIO=y' config-host.mak && make -j2 fio

FROM docker.io/library/debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libaio1t64=0.3.113-8+b1 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /tmp/fio/fio /usr/local/bin/fio
COPY --from=build /tmp/fio/COPYING /usr/share/doc/fio/COPYING
USER 65532:65532
HEALTHCHECK --interval=60s --timeout=5s --retries=2 CMD ["/usr/local/bin/fio", "--version"]
ENTRYPOINT ["/usr/local/bin/fio"]
