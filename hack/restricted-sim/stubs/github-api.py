#!/usr/bin/env python3
"""A one-repository GitHub API for restricted-sim: just enough of the
installation, rules and contents endpoints for fleet-discover to run end to
end offline, and a one-secret OpenBAO (login, one KV read, revoke) for
openbao-secrets. Serves until the container exits."""
import http.server
import json
import sys


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_PUT(self):
        if self.path == "/v1/auth/jwt-roster/login":
            return self.send(200, {"auth": {"client_token": "stub-client-token", "policies": ["default"]}})
        self.send(404, {"errors": ["not found"]})

    def do_POST(self):
        self.send(204, {})

    def do_GET(self):
        if self.path.startswith("/v1/kv/data/"):
            return self.send(200, {"data": {"data": {"STUB_SECRET": "stub-secret-value"}}})
        path = self.path.split("?")[0]
        if path == "/installation/repositories":
            return self.send(200, {"repositories": [
                {"full_name": "example/sim", "visibility": "private", "archived": False, "default_branch": "main"}]})
        if path == "/repos/example/sim":
            return self.send(200, {"default_branch": "main"})
        if "/rules/branches/" in path:
            return self.send(200, [{"type": "required_status_checks",
                                    "parameters": {"required_status_checks": [{"context": "check"}]}}])
        if "/contents/" in path:
            return self.send(200, {"name": "x"})
        self.send(404, {"message": "Not Found"})


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
