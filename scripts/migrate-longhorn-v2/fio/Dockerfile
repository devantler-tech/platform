FROM docker.io/library/debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392 AS build
SHELL ["/bin/sh", "-euxc"]
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc libc6-dev libaio-dev make pkgconf
COPY fio.tar.gz /tmp/fio.tar.gz
RUN printf '%s  %s\n' '5cacff5a41c4ad89c66f0348676156d56b680a3871c8d2da29a02d10cdef09ab' /tmp/fio.tar.gz | sha256sum --check && \
    mkdir /tmp/fio && tar --strip-components=1 -xzf /tmp/fio.tar.gz -C /tmp/fio && cd /tmp/fio && \
    ./configure --disable-native --extra-cflags='-march=x86-64 -mtune=generic' && \
    grep -qx 'CONFIG_LIBAIO=y' config-host.mak && make -j2 fio

FROM docker.io/library/debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libaio1t64 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /tmp/fio/fio /usr/local/bin/fio
COPY --from=build /tmp/fio/COPYING /usr/share/doc/fio/COPYING
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/fio"]
