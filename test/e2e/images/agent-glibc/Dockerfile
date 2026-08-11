FROM docker.io/library/python:3.11-slim-bookworm@sha256:77923445c077d8eb971b14b2b114a1d9cd4a87edb4c75654820ca4832ee8cb15

RUN groupadd --gid 65532 app \
    && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /work/app app \
    && mkdir -p /work/app \
    && chown 65532:65532 /work/app

COPY --chown=0:0 --chmod=0755 out/sandcamp-e2e-agent /usr/local/bin/sandcamp-e2e-agent

ENV SANDCAMP_E2E_IMAGE_ENV=from-agent-image

USER root
WORKDIR /
ENTRYPOINT ["/usr/local/bin/sandcamp-e2e-agent"]
