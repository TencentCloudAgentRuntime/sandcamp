FROM docker.io/library/python:3.11-slim-bookworm@sha256:77923445c077d8eb971b14b2b114a1d9cd4a87edb4c75654820ca4832ee8cb15 AS base

LABEL org.opencontainers.image.title="Sandcamp AGS validation main image" \
      org.opencontainers.image.description="Root and non-root main-image fixture with observable environment and shared-mount behavior"

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    SANDCAMP_MAIN_IMAGE_ENV=from-main-image

RUN mkdir -p \
      /mnt/sandcamp \
      /mnt/fastapi \
      /mnt/egress \
      /sandcamp-validation/share \
      /sandcamp-validation/config \
      /opt/sandcamp-validation \
    && printf '%s' 'readonly-from-main-image' \
      > /sandcamp-validation/config/probe.txt \
    && chmod 0777 /sandcamp-validation/share \
    && chmod 0444 /sandcamp-validation/config/probe.txt

COPY app.py /opt/sandcamp-validation/app.py

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/python", "/opt/sandcamp-validation/app.py"]

FROM base AS root
USER 0:0

FROM base AS nonroot
USER 65532:65532
