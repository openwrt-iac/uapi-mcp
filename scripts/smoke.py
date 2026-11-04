#!/usr/bin/env python3
"""End-to-end smoke for uapi-mcp against a real router running uapi.

The pre-tag gate from AGENTS.md, as one command: this drives the built binary
over stdio the way an MCP host does and asserts what the wire carries.

    UAPI_ENDPOINT=https://192.168.10.123/api/v3 \
    UAPI_TOKEN_FILE=/path/to/token.txt \
    make smoke

UAPI_TOKEN_FILE holds the bearer minted with uapi-token create; the token is
never printed. TLS verification is off by default (uapi's self-signed
certificate); pass UAPI_INSECURE=0 to verify. The binary defaults to
`uapi-mcp` from the PATH when the script is run directly; `make smoke` always
drives the binary `make build` just produced.

The script creates one DISABLED firewall rule, renames it, deletes it, and
leaves the router as it found it. It needs a token with firewall and network
write scopes, '*:rw' on a lab router.
"""

import json
import os
import subprocess
import sys

ENDPOINT = os.environ.get("UAPI_ENDPOINT", "")
TOKEN_FILE = os.environ.get("UAPI_TOKEN_FILE", "")
INSECURE = os.environ.get("UAPI_INSECURE", "1") not in ("0", "false")
BINARY = os.environ.get("UAPI_MCP_BINARY", "uapi-mcp")
EXPECTED_TOOLS = 266

checks = []


def ok(number, label):
    checks.append(label)
    print(f"{number}. {label}")


def fail(message):
    print(f"FAIL: {message}", file=sys.stderr)
    sys.exit(1)


if not ENDPOINT or not TOKEN_FILE:
    fail("UAPI_ENDPOINT and UAPI_TOKEN_FILE must be set; see the header of this script")
with open(TOKEN_FILE) as f:
    token = f.read().strip()
if not token:
    fail(f"no token in {TOKEN_FILE}")

env = dict(os.environ, UAPI_ENDPOINT=ENDPOINT, UAPI_TOKEN=token)
env["UAPI_INSECURE"] = "1" if INSECURE else "0"
proc = subprocess.Popen([BINARY], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE, env=env, text=True, bufsize=1)


def call(method, params):
    call.id += 1
    req = {"jsonrpc": "2.0", "id": call.id, "method": method, "params": params}
    proc.stdin.write(json.dumps(req) + "\n")
    proc.stdin.flush()
    while True:
        line = proc.stdout.readline()
        if not line:
            err = proc.stderr.readline().strip()
            fail(f"server closed stdout: {err}")
        msg = json.loads(line)
        if msg.get("id") == call.id:
            if "error" in msg:
                fail(f"{method}: protocol error {msg['error']}")
            return msg["result"]


call.id = 0


def tool(name, arguments):
    res = call("tools/call", {"name": name, "arguments": arguments})
    blocks = [c.get("text", "") for c in res.get("content", [])]
    return res.get("isError", False), blocks


# 1. initialize
result = call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                             "clientInfo": {"name": "smoke", "version": "0"}})
server = result["serverInfo"]
if server["name"] != "uapi-mcp":
    fail(f"serverInfo.name = {server['name']}")
ok(1, f"initialize ok: {server['name']} {server['version']}")
proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
proc.stdin.flush()

# 2. the whole catalog
tools = call("tools/list", {})["tools"]
if len(tools) != EXPECTED_TOOLS:
    fail(f"host sees {len(tools)} tools, want {EXPECTED_TOOLS}")
ok(2, f"tools/list ok: {len(tools)} tools")

# 3. a real read
err, blocks = tool("firewall_zones_list", {})
if err or not blocks or not isinstance(json.loads(blocks[0]), list) or len(json.loads(blocks[0])) < 1:
    fail(f"firewall_zones_list: err={err} text={blocks[:1]}")
zones = [z.get("name") for z in json.loads(blocks[0])]
ok(3, f"firewall_zones_list ok: {zones[:4]}")

# 4. a write arrives as payload block then footer block
err, blocks = tool("firewall_rules_create",
                  {"name": "mcp smoke", "target": "ACCEPT", "enabled": False})
if err:
    fail(f"firewall_rules_create: {blocks}")
if len(blocks) != 2:
    fail(f"write result carries {len(blocks)} blocks, want payload then footer")
created = json.loads(blocks[0])
if "X-Reload-Status" not in blocks[1]:
    fail(f"footer block without transaction headers: {blocks[1]!r}")
ok(4, f"firewall_rules_create ok: {created['id']} | payload block + footer block")

# 5. read it back
err, blocks = tool("firewall_rule_read", {"id": created["id"]})
if err or json.loads(blocks[0])["id"] != created["id"]:
    fail(f"firewall_rule_read: {blocks[:1]}")
ok(5, "firewall_rule_read ok")

# 6. merge-patch rename
err, blocks = tool("firewall_rule_merge",
                   {"id": created["id"], "body": {"name": "mcp smoke renamed"}})
if err or json.loads(blocks[0])["name"] != "mcp smoke renamed":
    fail(f"firewall_rule_merge: {blocks[:1]}")
ok(6, "firewall_rule_merge ok: renamed")

# 7. the 422 envelope teaches
err, blocks = tool("firewall_rules_create", {"name": "bogus", "target": "NOT_A_TARGET"})
if not err or "422" not in blocks[0] or "target" not in blocks[0]:
    fail(f"expected a 422 envelope naming the field: {blocks[:1]}")
ok(7, "422 envelope ok: field errors surfaced")

# 8. delete, then delete again
err, blocks = tool("firewall_rule_delete", {"id": created["id"]})
if err or "204" not in blocks[0]:
    fail(f"firewall_rule_delete: {blocks[:1]}")
err, blocks = tool("firewall_rule_delete", {"id": created["id"]})
if err or "already absent" not in blocks[0]:
    fail(f"second delete must be a 404 success: {blocks[:1]}")
err, blocks = tool("firewall_rule_read", {"id": created["id"]})
if not err or "404" not in blocks[0]:
    fail(f"read after delete must surface the 404: {blocks[:1]}")
ok(8, "delete cycle ok: 204, already-absent 404, 404 error result")

# 9. the empty collection answers [] (kernel peers need uapi >= 3.3.0)
err, blocks = tool("network_wireguard_kernel_peers_list", {})
if err and "404" in blocks[0]:
    print("9. wireguard kernel peers: endpoint absent on this uapi, skipped")
elif not err and blocks and blocks[0] == "[]":
    ok(9, "empty collection ok: []")
else:
    fail(f"network_wireguard_kernel_peers_list: {blocks[:1]}")

proc.stdin.close()
proc.wait(timeout=10)
print(f"SMOKE-OK ({len(checks)} checks)")
