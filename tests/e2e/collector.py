import json, socket, socketserver, sys, threading

MODE = sys.argv[1]          # "A" (cross-origin) or "B" (rebind relay)
PORT = int(sys.argv[2])
RPORT = int(sys.argv[3])    # router port (relay target, mode B)
RESULTS = sys.argv[4]       # results.log path
ATTACK = open(sys.argv[5], "rb").read()
RELAYLOG = sys.argv[6]

def record(k, v):
    with open(RESULTS, "a") as f:
        f.write(json.dumps({"k": k, "v": v}) + "\n")

def relay(head, body, conn):
    """Forward the browser's raw request to the router, preserving Host/Origin
    byte-for-byte, and stream the response back. The router uses keep-alive,
    so stop at Content-Length rather than waiting for EOF."""
    up = socket.create_connection(("127.0.0.1", RPORT), timeout=10)
    resp = b""
    try:
        up.sendall(head + body)
        while b"\r\n\r\n" not in resp:
            d = up.recv(65536)
            if not d:
                break
            resp += d
        rhead, rbody = resp.split(b"\r\n\r\n", 1)
        clen = None
        for line in rhead.split(b"\r\n"):
            if line.lower().startswith(b"content-length:"):
                clen = int(line.split(b":", 1)[1].strip())
        if clen is not None:
            while len(rbody) < clen:
                d = up.recv(65536)
                if not d:
                    break
                rbody += d
            resp = rhead + b"\r\n\r\n" + rbody
    finally:
        up.close()
        conn.sendall(resp)
        with open(RELAYLOG, "a") as f:
            f.write(json.dumps({
                "request_head": head.decode("latin-1"),
                "request_body": body.decode("utf-8", "replace"),
                "response_head": resp.split(b"\r\n\r\n", 1)[0].decode("latin-1"),
                "response_body": resp.split(b"\r\n\r\n", 1)[1].decode("utf-8", "replace") if b"\r\n\r\n" in resp else "",
            }) + "\n")

class C(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

class H(socketserver.BaseRequestHandler):
    def handle(self):
        conn = self.request
        conn.settimeout(10)
        buf = b""
        while b"\r\n\r\n" not in buf:
            d = conn.recv(65536)
            if not d:
                return
            buf += d
        head, rest = buf.split(b"\r\n\r\n", 1)
        first = head.split(b"\r\n", 1)[0].decode("latin-1")
        method, path, _ = first.split(" ", 2)
        clen = 0
        for line in head.split(b"\r\n")[1:]:
            if line.lower().startswith(b"content-length:"):
                clen = int(line.split(b":", 1)[1].strip())
        while len(rest) < clen:
            d = conn.recv(65536)
            if not d:
                break
            rest += d
        body = rest[:clen]

        if method == "GET" and path in ("/", "/attack.html"):
            resp = b"HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(ATTACK) + ATTACK
            conn.sendall(resp)
        elif method == "POST" and path == "/report":
            record("report", body.decode("utf-8", "replace"))
            conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
        elif path.startswith("/v1/"):
            if MODE == "B":
                relay(head + b"\r\n\r\n", body, conn)
            else:
                conn.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        else:
            conn.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        conn.close()

C(("0.0.0.0", PORT), H).serve_forever()
