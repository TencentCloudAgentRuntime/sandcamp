FROM docker.io/library/python:3.11-slim-bookworm@sha256:77923445c077d8eb971b14b2b114a1d9cd4a87edb4c75654820ca4832ee8cb15

LABEL org.opencontainers.image.title="Sandcamp FastAPI proxy fixture" \
      org.opencontainers.image.description="HTTP/WebSocket reverse proxy rootfs for Sandcamp Image Volume validation" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/WSH032/fastapi-proxy-lib"

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    FASTAPI_HOST=0.0.0.0 \
    FASTAPI_PORT=9200 \
    HOME=/root \
    UPSTREAM_URL=http://127.0.0.1:8080/ \
    SANDCAMP_FASTAPI_IMAGE_ENV=from-fastapi-image

WORKDIR /opt/fastapi-proxy

COPY requirements.txt .
RUN python -m pip install --no-cache-dir --requirement requirements.txt
RUN mkdir -p /mnt/share /etc/sandcamp-validation \
    && mkdir -p /home/app \
    && printf '%s\n' 'app:x:65532:65532:Sandcamp test user:/home/app:/usr/sbin/nologin' >> /etc/passwd \
    && printf '%s\n' 'app:x:65532:' >> /etc/group \
    && chown 65532:65532 /home/app \
    && chmod 0777 /home/app \
    && touch /etc/sandcamp-validation/probe.txt \
    && ln -s ../../outside /sandcamp-escape

COPY app.py upstream.py ws_check.py ./

EXPOSE 9200

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:9200/healthz', timeout=2).read()"]

ENTRYPOINT ["python", "/opt/fastapi-proxy/app.py"]
