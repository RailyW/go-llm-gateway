#!/usr/bin/env python3
"""极简 mock 上游，用于本地联调：POST /v1/chat/completions，支持 stream。"""
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        model = req.get("model", "?")
        auth = self.headers.get("authorization", "")
        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for word in ["mock", " stream", " from ", model]:
                chunk = {
                    "id": "chatcmpl-mock",
                    "object": "chat.completion.chunk",
                    "model": model,
                    "choices": [{"index": 0, "delta": {"content": word}, "finish_reason": None}],
                }
                self.wfile.write(self._sse(f"data: {json.dumps(chunk)}\n\n"))
                self.wfile.flush()
                time.sleep(0.05)
            final = {
                "id": "chatcmpl-mock",
                "object": "chat.completion.chunk",
                "model": model,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
            }
            self.wfile.write(self._sse(f"data: {json.dumps(final)}\n\n"))
            self.wfile.write(self._sse("data: [DONE]\n\n"))
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
            return

        body = json.dumps({
            "id": "chatcmpl-mock",
            "object": "chat.completion",
            "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant",
                                                 "content": f"mock reply for {model} (auth={auth[:12]}...)"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 9, "completion_tokens": 7, "total_tokens": 16},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    @staticmethod
    def _sse(text: str) -> bytes:
        data = text.encode()
        return f"{len(data):x}\r\n".encode() + data + b"\r\n"


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 9911), H).serve_forever()
