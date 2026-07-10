#!/usr/bin/env python3
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOST = "127.0.0.1"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18080

COMPACT_BODY = {
    "id": "resp_compact",
    "object": "response",
    "model": "gpt-5",
    "status": "completed",
    "created_at": 1700000000,
    "output": [{"type": "message", "content": [{"type": "output_text", "text": "hello compact"}]}],
    "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
}

SSE_COMPLETED = {
    "type": "response.completed",
    "response": {
        "id": "resp_sse",
        "object": "response",
        "model": "gpt-5",
        "status": "completed",
        "created_at": 1700000000,
        "output": [{"type": "message", "content": [{"type": "output_text", "text": "hello sse"}]}],
        "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
    },
}

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        _ = self.rfile.read(length) if length else b""
        if self.path == "/responses/compact":
            body = json.dumps(COMPACT_BODY).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/responses":
            chunks = [
                b'data: {"type":"response.created"}\n',
                b"data: " + json.dumps(SSE_COMPLETED).encode() + b"\n",
            ]
            total = b"".join(chunks)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(total)))
            self.end_headers()
            for chunk in chunks:
                self.wfile.write(chunk)
                self.wfile.flush()
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, format, *args):
        return

if __name__ == "__main__":
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
