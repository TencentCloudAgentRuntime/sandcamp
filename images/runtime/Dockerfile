FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e AS launcher-build

RUN apk add --no-cache build-base
COPY cmd/campd-launcher/main.c /src/main.c
RUN mkdir -p /out \
    && cc -static -Os -s -Wall -Wextra -Werror \
      -o /out/campd /src/main.c

FROM scratch

LABEL org.opencontainers.image.title="Sandcamp runtime bundle" \
      org.opencontainers.image.description="Restricted setuid bootstrap, static campd, and standalone sandrun for a read-only AGS Image Volume" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=launcher-build --chown=0:0 --chmod=4755 /out/campd /bin/campd
COPY --chown=0:0 --chmod=0755 out/campd /bin/campd.real
COPY --chown=0:0 --chmod=0755 out/sandrun /bin/sandrun

ENTRYPOINT ["/bin/campd"]
