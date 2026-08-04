#!/usr/bin/env python3
"""Stands in for the Struct8 API so you can read what the agent sends.

Point WORKER_BASE_URL at this instead of the real endpoint and every metric
batch the agent would upload gets printed to your terminal in full, with
nothing redacted. Requires only the Python standard library.

    python3 receiver.py          # listens on 0.0.0.0:8099

Requests are answered with {"skipped": 0}, which is what the real API returns
when it accepted every point.
"""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = 8099


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)

        print(f"\n=== POST {self.path} ===")
        # The agent authenticates with CLUSTER_API_KEY as a bearer token. It is
        # printed here so you can confirm it is the only credential in play.
        print(f"Authorization: {self.headers.get('Authorization')}")
        try:
            print(json.dumps(json.loads(raw), indent=2))
        except json.JSONDecodeError:
            print(raw.decode("utf-8", "replace"))

        body = json.dumps({"skipped": 0}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass  # the payload dump above is the only output worth reading


if __name__ == "__main__":
    print(f"Listening on 0.0.0.0:{PORT} — every batch will be printed below.")
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
