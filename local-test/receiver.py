#!/usr/bin/env python3
"""Stands in for both sides the agent talks to, and prints everything.

Two roles in one process, because the agent has exactly two counterparts:

  * the Struct8 API (WORKER_BASE_URL) -- receives the one outbound message the
    agent sends unasked, the announcement of its own address;
  * your Prometheus (PROMETHEUS_URL) -- receives a chart's query, and only when
    someone is looking at a chart.

Everything either one receives is printed in full, with nothing redacted, so
the interesting result is how LITTLE arrives: leave this running and the
terminal stays quiet until you ask for something. Requires only the Python
standard library.

    python3 receiver.py          # listens on 0.0.0.0:8099
"""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

PORT = 8099

# Two points, five minutes apart, so a chart has something to draw. The value
# is a string and the instant a number -- that is the Prometheus wire format,
# not a quirk of this file.
FAKE_SERIES = {
    "status": "success",
    "data": {
        "resultType": "matrix",
        "result": [
            {
                "metric": {"job": "local-test"},
                "values": [[1785975600, "0.25"], [1785975900, "0.5"]],
            }
        ],
    },
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urlparse(self.path)
        if not parsed.path.startswith("/api/v1/"):
            self._send(404, {"error": "not found"})
            return

        print(f"\n=== Prometheus query ===")
        for key, values in parse_qs(parsed.query).items():
            for value in values:
                print(f"{key}: {value}")
        self._send(200, FAKE_SERIES)

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

        self._send(200, {"ok": True})

    def _send(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass  # the dumps above are the only output worth reading


if __name__ == "__main__":
    print(f"Listening on 0.0.0.0:{PORT} — everything received will be printed below.")
    print("Silence means the agent is sending nothing, which is the normal state.")
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
