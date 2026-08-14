#!/usr/bin/env python3
"""极简 mock 上游，用于本地联调三种端点（均为同协议直转的目标）：

  POST /v1/chat/completions   OpenAI chat（Bearer 鉴权）
  POST /v1/responses          OpenAI responses（Bearer 鉴权）
  POST /v1/messages           Anthropic messages（x-api-key + anthropic-version）

响应里会回显收到的鉴权头，便于确认网关按协议换对了 header。
"""
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 9911


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    # ---------- 基础工具 ----------
    def _read(self):
        n = int(self.headers.get("content-length", 0))
        return json.loads(self.rfile.read(n) or b"{}")

    def _auth_echo(self):
        return {
            "authorization": self.headers.get("authorization", ""),
            "x-api-key": self.headers.get("x-api-key", ""),
            "anthropic-version": self.headers.get("anthropic-version", ""),
        }

    def _json(self, obj, status=200):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _sse_start(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

    def _sse(self, text):
        data = text.encode()
        self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
        self.wfile.flush()

    def _sse_end(self):
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    # ---------- 路由 ----------
    def do_POST(self):
        req = self._read()
        model = req.get("model", "?")
        stream = bool(req.get("stream"))
        path = self.path.rstrip("/")

        if path.endswith("/chat/completions"):
            return self.chat(model, stream)
        if path.endswith("/responses"):
            return self.openai_responses(model, stream)
        if path.endswith("/messages"):
            return self.messages(model, stream)
        return self._json({"error": {"message": f"mock: unknown path {self.path}"}}, 404)

    # ---------- OpenAI /chat/completions ----------
    def chat(self, model, stream):
        if not stream:
            return self._json({
                "id": "chatcmpl-mock", "object": "chat.completion", "model": model,
                "choices": [{"index": 0, "finish_reason": "stop", "message": {
                    "role": "assistant", "content": f"[chat] {model} auth={self._auth_echo()}"}}],
                "usage": {"prompt_tokens": 9, "completion_tokens": 7, "total_tokens": 16},
            })
        self._sse_start()
        for w in ["[chat] ", "stream ", model]:
            self._sse("data: " + json.dumps({
                "id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
                "choices": [{"index": 0, "delta": {"content": w}, "finish_reason": None}],
            }) + "\n\n")
            time.sleep(0.03)
        self._sse("data: " + json.dumps({
            "id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14},
        }) + "\n\n")
        self._sse("data: [DONE]\n\n")
        self._sse_end()

    # ---------- OpenAI /responses ----------
    def openai_responses(self, model, stream):
        if not stream:
            return self._json({
                "id": "resp_mock", "object": "response", "model": model, "status": "completed",
                "output": [{"type": "message", "role": "assistant",
                            "content": [{"type": "output_text",
                                         "text": f"[responses] {model} auth={self._auth_echo()}"}]}],
                "usage": {"input_tokens": 12, "output_tokens": 8, "total_tokens": 20},
            })
        self._sse_start()
        for w in ["[responses] ", "stream ", model]:
            self._sse("event: response.output_text.delta\n" + "data: " + json.dumps(
                {"type": "response.output_text.delta", "delta": w}) + "\n\n")
            time.sleep(0.03)
        self._sse("event: response.completed\n" + "data: " + json.dumps({
            "type": "response.completed",
            "response": {"id": "resp_mock", "model": model, "status": "completed",
                         "usage": {"input_tokens": 13, "output_tokens": 6, "total_tokens": 19}},
        }) + "\n\n")
        self._sse_end()

    # ---------- Anthropic /messages ----------
    def messages(self, model, stream):
        if not stream:
            return self._json({
                "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                "content": [{"type": "text", "text": f"[messages] {model} auth={self._auth_echo()}"}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 14, "output_tokens": 9},
            })
        self._sse_start()
        self._sse("event: message_start\n" + "data: " + json.dumps({
            "type": "message_start",
            "message": {"id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                        "content": [], "usage": {"input_tokens": 15, "output_tokens": 1}},
        }) + "\n\n")
        for w in ["[messages] ", "stream ", model]:
            self._sse("event: content_block_delta\n" + "data: " + json.dumps({
                "type": "content_block_delta", "index": 0,
                "delta": {"type": "text_delta", "text": w}}) + "\n\n")
            time.sleep(0.03)
        self._sse("event: message_delta\n" + "data: " + json.dumps({
            "type": "message_delta", "delta": {"stop_reason": "end_turn"},
            "usage": {"output_tokens": 21}}) + "\n\n")
        self._sse("event: message_stop\n" + 'data: {"type":"message_stop"}\n\n')
        self._sse_end()


if __name__ == "__main__":
    print(f"mock upstream on http://127.0.0.1:{PORT} "
          f"(/v1/chat/completions, /v1/responses, /v1/messages)")
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
