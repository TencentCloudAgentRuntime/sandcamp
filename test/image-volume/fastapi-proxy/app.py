import errno
import hashlib
import os
from urllib.parse import urlsplit

import uvicorn
from fastapi import FastAPI
from fastapi_proxy_lib.core.http import ReverseHttpProxy
from fastapi_proxy_lib.core.websocket import ReverseWebSocketProxy
from fastapi_proxy_lib.fastapi.router import RouterHelper


def upstream_url() -> str:
    value = os.getenv("UPSTREAM_URL", "http://127.0.0.1:8080/").strip()
    parsed = urlsplit(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise RuntimeError("UPSTREAM_URL must be an absolute HTTP(S) URL")
    return value if value.endswith("/") else value + "/"


def websocket_url(value: str) -> str:
    parsed = urlsplit(value)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return parsed._replace(scheme=scheme).geturl()


def write_sidecar_evidence() -> dict:
    path = "/mnt/share/sidecar-created.txt"
    try:
        with open(path, "w", encoding="utf-8") as output:
            output.write("created-by-fastapi")
        metadata = os.stat(path)
        return {
            "written": True,
            "error": None,
            "uid": metadata.st_uid,
            "gid": metadata.st_gid,
        }
    except OSError as error:
        return {
            "written": False,
            "error": f"{error.errno}:{error.strerror}",
            "uid": None,
            "gid": None,
        }


def read_text(path: str) -> str | None:
    try:
        with open(path, encoding="utf-8") as source:
            return source.read()
    except OSError:
        return None


def file_identity(path: str) -> dict | None:
    try:
        metadata = os.stat(path)
        return {
            "uid": metadata.st_uid,
            "gid": metadata.st_gid,
            "mode": oct(metadata.st_mode & 0o7777),
        }
    except OSError:
        return None


def write_access_errno(path: str) -> int | None:
    try:
        with open(path, "a", encoding="utf-8"):
            pass
        return None
    except OSError as error:
        return error.errno


def root_filesystem_type() -> str | None:
    mountinfo = read_text("/proc/self/mountinfo")
    if mountinfo is None:
        return None
    for line in mountinfo.splitlines():
        left, separator, right = line.partition(" - ")
        fields = left.split()
        if separator and len(fields) > 4 and fields[4] == "/":
            return right.split()[0]
    return None


def selected_environment_evidence() -> dict | None:
    selected = os.getenv("SANDCAMP_ENV_VALIDATION_NAMES")
    if selected is None:
        return None
    names = selected.split(",") if selected else []
    digest = hashlib.sha256()
    missing = []
    total_value_bytes = 0
    for name in names:
        value = os.getenv(name)
        if value is None:
            missing.append(name)
            continue
        encoded_name = name.encode()
        encoded_value = value.encode()
        total_value_bytes += len(encoded_value)
        digest.update(encoded_name)
        digest.update(b"\0")
        digest.update(encoded_value)
        digest.update(b"\0")
    return {
        "variable_count": len(names),
        "total_value_bytes": total_value_bytes,
        "sha256": digest.hexdigest(),
        "missing_count": len(missing),
    }


def write_overlay_evidence() -> dict:
    paths = [
        "/var/lib/sandcamp-validation/cache.txt",
        os.path.join(os.environ.get("HOME", "/root"), ".cache/sandcamp-validation.txt"),
        "/etc/sandcamp-overlay.txt",
    ]
    results = {}
    for path in paths:
        try:
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w", encoding="utf-8") as output:
                output.write("written-to-overlay")
            results[path] = {"written": True, "error": None}
        except OSError as error:
            results[path] = {
                "written": False,
                "error": f"{error.errno}:{error.strerror}",
            }
    return {
        "root_filesystem": root_filesystem_type(),
        "writes": results,
    }


sidecar_file = write_sidecar_evidence()
overlay_evidence = write_overlay_evidence()
upstream = upstream_url()
router_helper = RouterHelper()
http_router = router_helper.register_router(ReverseHttpProxy(base_url=upstream))
websocket_router = router_helper.register_router(
    ReverseWebSocketProxy(base_url=websocket_url(upstream))
)
app = FastAPI(
    lifespan=router_helper.get_lifespan(),
    docs_url=None,
    redoc_url=None,
    openapi_url=None,
)


@app.get("/healthz")
async def healthz():
    return {"status": "ok"}


@app.get("/sandcamp/evidence")
async def sandcamp_evidence():
    readonly_path = "/etc/sandcamp-validation/probe.txt"
    readonly_error = None
    try:
        with open(readonly_path, "a", encoding="utf-8") as output:
            output.write("unexpected-write")
    except OSError as error:
        readonly_error = error.errno
    return {
        "process_env": os.getenv("SANDCAMP_SIDECAR_ENV"),
        "main_image_env": os.getenv("SANDCAMP_MAIN_IMAGE_ENV"),
        "image_config_env": os.getenv("SANDCAMP_FASTAPI_IMAGE_ENV"),
        "environment_profile": selected_environment_evidence(),
        "uid": os.getuid(),
        "gid": os.getgid(),
        "cwd": os.getcwd(),
        "overlay": overlay_evidence,
        "shared_write": sidecar_file,
        "shared_main_file": read_text("/mnt/share/main-created.txt"),
        "shared_main_identity": file_identity("/mnt/share/main-created.txt"),
        "shared_main_write_errno": write_access_errno(
            "/mnt/share/main-created.txt"
        ),
        "share_identity": file_identity("/mnt/share"),
        "readonly_config": read_text(readonly_path),
        "readonly_write_blocked": readonly_error
        in {errno.EACCES, errno.EPERM, errno.EROFS},
    }


app.include_router(http_router)
app.include_router(websocket_router)


if __name__ == "__main__":
    uvicorn.run(
        app,
        host=os.getenv("FASTAPI_HOST", "0.0.0.0"),
        port=int(os.getenv("FASTAPI_PORT", "9200")),
        log_level=os.getenv("LOG_LEVEL", "info"),
        proxy_headers=False,
    )
