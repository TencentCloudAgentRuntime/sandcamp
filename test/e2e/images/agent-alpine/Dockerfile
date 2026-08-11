FROM docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e

RUN apk add --no-cache iptables nftables \
    && addgroup -g 65532 app \
    && adduser -D -H -u 65532 -G app app \
    && mkdir -p /work/app \
    && chown 65532:65532 /work/app

COPY --chown=0:0 --chmod=0755 out/sandcamp-e2e-agent /usr/local/bin/sandcamp-e2e-agent

ENV SANDCAMP_E2E_IMAGE_ENV=from-agent-image

USER root
WORKDIR /
ENTRYPOINT ["/usr/local/bin/sandcamp-e2e-agent"]
