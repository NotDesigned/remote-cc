#!/usr/bin/env python3
"""A stand-in for the Anthropic Messages API, for end-to-end tests of real
Claude Code without an account.

A request whose tools include Bash gets Bash tool calls running each COMMAND
in turn; each result that comes back is appended to LOG (one JSON line), and
after the last one the reply is the text "DONE". Every other request (titles,
summaries, ...) gets a short text reply.

  mockapi.py --listen 0.0.0.0:8765 --command 'uname -n' --command pwd --log /tmp/results
"""
import argparse
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ap = argparse.ArgumentParser()
ap.add_argument("--listen", default="127.0.0.1:8765")
ap.add_argument("--command", action="append", required=True)
ap.add_argument("--log", required=True)
args = ap.parse_args()


def tool_results(messages):
    out = []
    for m in messages:
        if m.get("role") != "user" or not isinstance(m.get("content"), list):
            continue
        for b in m["content"]:
            if b.get("type") == "tool_result":
                c = b.get("content")
                if isinstance(c, list):
                    c = "".join(x.get("text", "") for x in c if isinstance(x, dict))
                out.append({"is_error": bool(b.get("is_error")), "content": c})
    return out


def reply_for(req):
    tools = [t.get("name") for t in req.get("tools") or []]
    if "Bash" not in tools:
        return [{"type": "text", "text": "ok"}], "end_turn"
    results = tool_results(req.get("messages") or [])
    if results:
        with open(args.log, "a") as f:
            f.write(json.dumps(results[-1]) + "\n")
    n = len(results)
    if n < len(args.command):
        return [{"type": "tool_use", "id": f"toolu_mock_{n + 1}", "name": "Bash",
                 "input": {"command": args.command[n], "description": "mock test command"}}], "tool_use"
    return [{"type": "text", "text": "DONE"}], "end_turn"


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):
        sys.stderr.write("mockapi: " + (fmt % a) + "\n")

    def send(self, code, body, ctype="application/json"):
        b = body.encode()
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        self.send(404, json.dumps({"type": "error", "error": {"type": "not_found_error", "message": "mock"}}))

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        path = self.path.split("?")[0]
        if path.endswith("/count_tokens"):
            return self.send(200, json.dumps({"input_tokens": 10}))
        if not path.endswith("/v1/messages"):
            return self.do_GET()
        blocks, stop = reply_for(req)
        msg = {"id": "msg_mock", "type": "message", "role": "assistant", "model": req.get("model", "mock"),
               "content": blocks, "stop_reason": stop, "stop_sequence": None,
               "usage": {"input_tokens": 10, "output_tokens": 5}}
        if not req.get("stream"):
            return self.send(200, json.dumps(msg))
        ev = []

        def e(t, d):
            d["type"] = t
            ev.append(f"event: {t}\ndata: {json.dumps(d)}\n\n")
        e("message_start", {"message": dict(msg, content=[], stop_reason=None)})
        for i, b in enumerate(blocks):
            if b["type"] == "text":
                e("content_block_start", {"index": i, "content_block": {"type": "text", "text": ""}})
                e("content_block_delta", {"index": i, "delta": {"type": "text_delta", "text": b["text"]}})
            else:
                e("content_block_start", {"index": i, "content_block": dict(b, input={})})
                e("content_block_delta", {"index": i, "delta": {"type": "input_json_delta",
                                                                  "partial_json": json.dumps(b["input"])}})
            e("content_block_stop", {"index": i})
        e("message_delta", {"delta": {"stop_reason": stop, "stop_sequence": None}, "usage": {"output_tokens": 5}})
        e("message_stop", {})
        self.send(200, "".join(ev), "text/event-stream")


host, port = args.listen.rsplit(":", 1)
ThreadingHTTPServer((host, int(port)), H).serve_forever()
