import errno
import json
import os

import uvicorn


def read_text(path):
    try:
        with open(path, encoding="utf-8") as source:
            return source.read()
    except OSError:
        return None


try:
    with open("/mnt/share/main-created.txt", "w", encoding="utf-8") as output:
        output.write("created-by-main")
    main_write_error = None
except OSError as error:
    main_write_error = f"{error.errno}:{error.strerror}"


async def app(scope, receive, send):
    if scope["type"] == "http":
        if scope["path"] == "/sandcamp/main-evidence":
            readonly_error = None
            try:
                with open(
                    "/etc/sandcamp-validation/probe.txt", "a", encoding="utf-8"
                ) as output:
                    output.write("unexpected-write")
            except OSError as error:
                readonly_error = error.errno
            body = json.dumps(
                {
                    "main_image_env": os.getenv("SANDCAMP_MAIN_IMAGE_ENV"),
                    "main_process_env": os.getenv("SANDCAMP_MAIN_PROCESS_ENV"),
                    "uid": os.getuid(),
                    "gid": os.getgid(),
                    "main_write_error": main_write_error,
                    "main_file": read_text("/mnt/share/main-created.txt"),
                    "sidecar_file": read_text("/mnt/share/sidecar-created.txt"),
                    "readonly_config": read_text(
                        "/etc/sandcamp-validation/probe.txt"
                    ),
                    "readonly_write_blocked": readonly_error
                    in {errno.EACCES, errno.EPERM, errno.EROFS},
                },
                separators=(",", ":"),
            ).encode()
        else:
            body = b'{"status":"upstream"}'
        await send(
            {
                "type": "http.response.start",
                "status": 200,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"content-length", str(len(body)).encode("ascii")),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body})
        return
    if scope["type"] == "websocket":
        await send({"type": "websocket.accept"})
        while True:
            message = await receive()
            if message["type"] == "websocket.disconnect":
                return
            if message["type"] == "websocket.receive":
                await send(
                    {
                        "type": "websocket.send",
                        "text": message.get("text"),
                        "bytes": message.get("bytes"),
                    }
                )
        return
    if scope["type"] == "lifespan":
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                await send({"type": "lifespan.shutdown.complete"})
                return


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=8080, proxy_headers=False)
