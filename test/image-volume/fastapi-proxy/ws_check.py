import sys

from httpx_ws import connect_ws


with connect_ws(sys.argv[1]) as websocket:
    websocket.send_text("sandcamp-websocket")
    received = websocket.receive_text()

if received != "sandcamp-websocket":
    raise RuntimeError(f"unexpected echo: {received!r}")

print(received)
