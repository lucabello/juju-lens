#!/usr/bin/env python3
"""Minimal Pebble API stub on a unix socket, for the tap's ops version matrix.

GET system-info succeeds, GET services is a 404, and pushing a file or adding
a layer succeeds without keeping anything.
"""
import json
import os
import socketserver
import sys
from http.server import BaseHTTPRequestHandler


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, result):
        body = json.dumps({'type': 'sync' if code == 200 else 'error', 'status-code': code,
                           'status': 'OK' if code == 200 else 'Not Found',
                           'result': result}).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith('/v1/system-info'):
            self._send(200, {'version': '1.10.0'})
        else:
            self._send(404, {'message': 'not found: ' + self.path})

    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        if n:
            self.rfile.read(n)
        elif self.headers.get('Transfer-Encoding') == 'chunked':
            while True:
                size = int(self.rfile.readline().strip() or b'0', 16)
                self.rfile.read(size + 2)
                if size == 0:
                    break
        if self.path.startswith('/v1/files'):
            self._send(200, [{'path': '/etc/fake.conf'}])
        elif self.path.startswith('/v1/layers'):
            self._send(200, True)
        else:
            self._send(404, {'message': 'not found: ' + self.path})


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        req, _ = super().get_request()
        return req, ('local', 0)


if __name__ == '__main__':
    path = sys.argv[1]
    if os.path.exists(path):
        os.unlink(path)
    Server(path, Handler).serve_forever()
