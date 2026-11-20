# Demo service for scripts/e2e.sh. "baseline" is the build on main; "candidate" is a
# PR build with three seeded bugs (field removed, type changed, value changed),
# one additive field, and two fields that change on every call (ts, etag).
import http.server, json, sys, time, itertools
port, mode = int(sys.argv[1]), sys.argv[2]   # mode: baseline | candidate
counter = itertools.count()
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        p = self.path.split('?')[0]
        parts = p.strip('/').split('/')
        body, status = None, 200
        if len(parts) == 2 and parts[0] == 'users':
            n = int(parts[1])
            body = {"id": n, "name": f"user {n}", "age": 20 + n, "ts": time.time_ns() + next(counter), "tags": ["a", "b"]}
            if mode == 'candidate':
                if n == 3: del body["name"]            # seeded bug 1: field removed
                if n == 5: body["age"] = str(body["age"])  # seeded bug 2: type changed
        elif len(parts) == 2 and parts[0] == 'orders':
            n = int(parts[1])
            body = {"id": n, "total": 10.0 * n, "etag": f"e{time.time_ns()}", "items": [{"sku": "x", "qty": 1}, {"sku": "y", "qty": 2}]}
            if mode == 'candidate' and n == 2: body["total"] = 99.0   # seeded bug 3: value changed
            if mode == 'candidate': body["currency"] = "USD"          # additive change on every order
        else:
            status, body = 404, {"error": "not found"}
        out = json.dumps(body).encode()
        self.send_response(status); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(out))); self.end_headers(); self.wfile.write(out)
http.server.ThreadingHTTPServer(('127.0.0.1', port), H).serve_forever()
