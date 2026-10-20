import http.server, json, sys

LOG = sys.argv[2]

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _log(self, body):
        with open(LOG, "a") as f:
            f.write(json.dumps({
                "path": self.path,
                "headers": dict(self.headers),
                "body": body.decode("utf-8", "replace"),
            }) + "\n")

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        self._log(body)
        resp = b'{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"}}]}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)

    def do_GET(self):
        self._log(b"")
        b = b"ok"
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def log_message(self, *a):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
