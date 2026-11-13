"""Minimal stand-in for the NordVPN API, for the Docker integration test.

Usage: python3 fakeapi.py <config.json>
config.json: {"token": "...", "client_private_key": "...", "servers": [
  {"hostname": "...", "ip": "...", "public_key": "...", "country": "...", "code": "..", "city": "..."}]}
"""
import base64
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CFG = json.load(open(sys.argv[1]))


def server_json(s, i):
    return {
        "id": i, "name": s["name"], "station": s["ip"], "hostname": s["hostname"],
        "load": 10 + i, "status": "online",
        "locations": [{"country": {"id": 208, "name": s["country"], "code": s["code"],
                                    "city": {"id": 7852919, "name": s["city"]}}}],
        "technologies": [{"identifier": "wireguard_udp",
                          "metadata": [{"name": "public_key", "value": s["public_key"]}]}],
    }


class H(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/v1/users/services/credentials":
            auth = self.headers.get("Authorization", "")
            want = "Basic " + base64.b64encode(("token:" + CFG["token"]).encode()).decode()
            if auth != want:
                return self._send(401, {"errors": {"message": "Unauthorized"}})
            return self._send(200, {"username": "u", "password": "p",
                                    "nordlynx_private_key": CFG["client_private_key"]})
        if path == "/v1/servers/recommendations":
            return self._send(200, [server_json(s, i) for i, s in enumerate(CFG["servers"])])
        if path == "/v1/servers/countries":
            return self._send(200, [{"id": 208, "name": "Sweden", "code": "SE", "serverCount": 2,
                                     "cities": [{"id": 7852919, "name": "Stockholm", "serverCount": 2}]}])
        if path == "/v1/helpers/ips/insights":
            return self._send(200, {"ip": self.client_address[0], "country": "Sweden",
                                    "city": "Stockholm", "isp": "sim", "protected": True})
        self._send(404, {})

    def log_message(self, fmt, *args):
        sys.stderr.write("api: " + fmt % args + "\n")


ThreadingHTTPServer(("0.0.0.0", 8000), H).serve_forever()
