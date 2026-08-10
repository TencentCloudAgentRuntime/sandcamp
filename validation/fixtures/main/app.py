import base64
import errno
import hashlib
import http.client
import json
import os
import socket
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


SHARE = "/sandcamp-validation/share"
MAIN_FILE = f"{SHARE}/main-created.txt"
SIDECAR_FILE = f"{SHARE}/sidecar-created.txt"
EGRESS_RULES_FILE = f"{SHARE}/egress-iptables-save.txt"
RUNTIME_WRITE_PROBE = "/mnt/sandcamp/.sandcamp-validation-write"
OVERLAY_LOWER_PATHS = [
    "/mnt/fastapi/var/lib/sandcamp-validation/cache.txt",
    "/mnt/fastapi/root/.cache/sandcamp-validation.txt",
    "/mnt/fastapi/etc/sandcamp-overlay.txt",
]
WEBSOCKET_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
EGRESS_POLICY_HOST = "127.0.0.1"
EGRESS_POLICY_PORT = 24774
EGRESS_ALLOWED_DOMAIN = "example.com"
EGRESS_DENIED_DOMAIN = "example.org"


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


def effective_capabilities() -> str | None:
    status = read_text("/proc/self/status")
    if status is None:
        return None
    for line in status.splitlines():
        if line.startswith("CapEff:"):
            return line.split(":", 1)[1].strip()
    return None


def process_identity(pid: int) -> dict | None:
    status = read_text(f"/proc/{pid}/status")
    if status is None:
        return None
    selected = {}
    for line in status.splitlines():
        key, separator, value = line.partition(":")
        if separator and key in {"Uid", "Gid", "Groups", "CapEff", "NoNewPrivs"}:
            selected[key] = value.strip()
    return selected


def egress_rule_evidence() -> dict:
    rules = read_text(EGRESS_RULES_FILE)
    if rules is None:
        return {
            "captured": False,
            "dns_redirect": False,
            "redirect_target": False,
            "selected_rules": [],
        }
    selected = [
        line
        for line in rules.splitlines()
        if "--dport 53" in line or "-j REDIRECT" in line or "-j DNAT" in line
    ][:12]
    return {
        "captured": True,
        "dns_redirect": "--dport 53" in rules,
        "redirect_target": "-j REDIRECT" in rules or "-j DNAT" in rules,
        "selected_rules": selected,
    }


def resolve_domain(host: str) -> dict:
    try:
        addresses = sorted(
            {
                address[0]
                for _, _, _, _, address in socket.getaddrinfo(
                    host, 443, type=socket.SOCK_STREAM
                )
            }
        )
        return {
            "host": host,
            "resolved": bool(addresses),
            "addresses": addresses[:8],
            "error": None,
        }
    except OSError as error:
        return {
            "host": host,
            "resolved": False,
            "addresses": [],
            "error": f"{type(error).__name__}:{error}",
        }


def egress_policy_evidence() -> dict:
    policy = None
    policy_error = None
    connection = http.client.HTTPConnection(
        EGRESS_POLICY_HOST, EGRESS_POLICY_PORT, timeout=2
    )
    try:
        connection.request("GET", "/policy")
        response = connection.getresponse()
        body = response.read(64 * 1024)
        if response.status == 200:
            policy = json.loads(body)
        else:
            policy_error = f"http-status:{response.status}"
    except (OSError, ValueError, json.JSONDecodeError) as error:
        policy_error = f"{type(error).__name__}:{error}"
    finally:
        connection.close()
    return {
        "policy": policy,
        "policy_error": policy_error,
        "allowed": resolve_domain(EGRESS_ALLOWED_DOMAIN),
        "denied": resolve_domain(EGRESS_DENIED_DOMAIN),
    }


def read_exact(source, size: int) -> bytes:
    value = source.read(size)
    if len(value) != size:
        raise ConnectionError("unexpected end of WebSocket frame")
    return value


def read_websocket_text(source) -> bytes | None:
    first, second = read_exact(source, 2)
    opcode = first & 0x0F
    if opcode == 0x08:
        return None
    if first & 0x80 == 0 or opcode != 0x01:
        raise ValueError(f"unsupported WebSocket frame opcode={opcode}")
    length = second & 0x7F
    if length == 126:
        length = int.from_bytes(read_exact(source, 2), "big")
    elif length == 127:
        length = int.from_bytes(read_exact(source, 8), "big")
    if length > 64 * 1024:
        raise ValueError("WebSocket validation frame is too large")
    if second & 0x80 == 0:
        raise ValueError("client WebSocket frame is not masked")
    mask = read_exact(source, 4)
    payload = bytearray(read_exact(source, length))
    for index in range(len(payload)):
        payload[index] ^= mask[index % len(mask)]
    return bytes(payload)


def write_websocket_text(destination, payload: bytes):
    header = bytearray([0x81])
    if len(payload) < 126:
        header.append(len(payload))
    elif len(payload) <= 0xFFFF:
        header.append(126)
        header.extend(len(payload).to_bytes(2, "big"))
    else:
        header.append(127)
        header.extend(len(payload).to_bytes(8, "big"))
    destination.write(header + payload)
    destination.flush()


try:
    with open(MAIN_FILE, "w", encoding="utf-8") as output:
        output.write("created-by-main")
    main_write_error = None
except OSError as error:
    main_write_error = f"{error.errno}:{error.strerror}"

try:
    with open(RUNTIME_WRITE_PROBE, "w", encoding="utf-8") as output:
        output.write("unexpected-write")
    os.unlink(RUNTIME_WRITE_PROBE)
    runtime_volume_write_errno = None
except OSError as error:
    runtime_volume_write_errno = error.errno


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if (
            self.path == "/echo"
            and self.headers.get("Upgrade", "").lower() == "websocket"
        ):
            self.websocket_echo()
            return
        if self.path == "/healthz":
            self.respond({"status": "ok"})
            return
        if self.path == "/sandcamp/main-evidence":
            self.respond(
                {
                    "main_image_env": os.getenv("SANDCAMP_MAIN_IMAGE_ENV"),
                    "instance_env": os.getenv("SANDCAMP_INSTANCE_ENV"),
                    "main_process_env": os.getenv("SANDCAMP_MAIN_PROCESS_ENV"),
                    "uid": os.getuid(),
                    "gid": os.getgid(),
                    "groups": os.getgroups(),
                    "effective_capabilities": effective_capabilities(),
                    "process_identity": process_identity(os.getpid()),
                    "pid1_executable": safe_readlink("/proc/1/exe"),
                    "pid1_identity": process_identity(1),
                    "main_write_error": main_write_error,
                    "main_file": read_text(MAIN_FILE),
                    "main_file_identity": file_identity(MAIN_FILE),
                    "sidecar_file": read_text(SIDECAR_FILE),
                    "sidecar_file_identity": file_identity(SIDECAR_FILE),
                    "sidecar_file_write_errno": write_access_errno(SIDECAR_FILE),
                    "share_identity": file_identity(SHARE),
                    "egress_rules": egress_rule_evidence(),
                    "egress_policy": egress_policy_evidence(),
                    "runtime_volume_write_errno": runtime_volume_write_errno,
                    "runtime_volume_write_blocked": runtime_volume_write_errno
                    in {errno.EACCES, errno.EPERM, errno.EROFS},
                    "overlay_lower_paths": {
                        path: os.path.lexists(path) for path in OVERLAY_LOWER_PATHS
                    },
                    "overlay_lower_unchanged": not any(
                        os.path.lexists(path) for path in OVERLAY_LOWER_PATHS
                    ),
                }
            )
            return
        self.respond({"status": "upstream"})

    def websocket_echo(self):
        key = self.headers.get("Sec-WebSocket-Key")
        if key is None:
            self.send_error(400, "missing Sec-WebSocket-Key")
            return
        accept = base64.b64encode(
            hashlib.sha1((key + WEBSOCKET_GUID).encode()).digest()
        ).decode()
        self.send_response(101)
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.end_headers()
        payload = read_websocket_text(self.rfile)
        if payload is not None:
            write_websocket_text(self.wfile, payload)
        self.close_connection = True

    def respond(self, value: dict):
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return


def safe_readlink(path: str) -> str | None:
    try:
        return os.readlink(path)
    except OSError:
        return None


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
