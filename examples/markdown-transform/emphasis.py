#!/usr/bin/env python3
"""Display-only status pills served over newline-delimited JSON-RPC 2.0.

Code spans and fenced blocks stay unchanged.
"""
import json
import re
import sys

COLORS = {"Done": 32, "Passed": 32, "Failed": 31, "Blocked": 31, "Needs you": 33}
TOKEN = re.compile(r"(`+)(.*?)\1|\[\[(Done|Passed|Failed|Blocked|Needs you)\]\]")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")


def decorate(markdown):
    lines = []
    fence = None
    for line in markdown.splitlines(keepends=True):
        match = FENCE.match(line)
        if fence:
            if match and match[1][0] == fence[0] and len(match[1]) >= len(fence):
                fence = None
        elif match:
            fence = match[1]
        else:
            line = TOKEN.sub(
                lambda m: m[0] if m[1] else f"\033[{COLORS[m[3]]};7m {m[3]} \033[27;39m",
                line,
            )
        lines.append(line)
    return "".join(lines)


def reply(request):
    if request.get("method") != "markdown/transform":
        return {"error": {"code": -32601, "message": "method not found"}}
    params = request["params"]
    markdown = params["markdown"]
    if params["message_type"] == "assistant":
        markdown = decorate(markdown)
    return {"result": {"markdown": markdown}}


for line in sys.stdin:
    request = json.loads(line)
    if "id" in request:
        print(json.dumps({"jsonrpc": "2.0", "id": request["id"], **reply(request)}), flush=True)
