# uapi-mcp

MCP server that exposes uapi, the native HTTP REST API of OpenWrt, to LLM agents as tools. Runs locally on the agent's host as a stdio process spawned by the MCP client, and talks to a router's uapi endpoint over HTTP. Go, one static binary, no runtime state of its own.

Third member of the openwrt-iac family: uapi is the server, terraform-provider-uapi drives it from IaC, uapi-mcp drives it from an agent's session. Module path: `github.com/openwrt-iac/uapi-mcp`.

This file is the founding document and the every-turn meta document: project identity, non-negotiables, tool design, and pointers to the family.

## Non-negotiables

1. **uapi over HTTP only.** The server consumes the curated REST endpoints of the router's uapi. No ssh, no uci, no ubus, no `/etc/config` access. The API is the only supported path to the box.
2. **Local stdio process.** The MCP host (OpenCode, Claude Code, gh-aw) spawns the binary and owns its lifecycle; stdio is the only transport. No daemon, no listening socket, no transport auth to design.
3. **Same client discipline as the provider.** Its four forward-compatibility invariants, verbatim:
   - read responses into a map and pick known keys; never strict-decode into structs that reject unknown fields, uapi adds response fields within a major
   - no client-side enum validation; `proto`, `target` and friends are strings checked server-side, and uapi may add values within a major
   - branch on HTTP status, not on the error `code` string
   - never hardcode the API major; the version lives in the user-supplied endpoint path
4. **Credentials from the environment, same names and semantics as the provider:** `UAPI_ENDPOINT`, `UAPI_TOKEN`, `UAPI_INSECURE`. The token carries the authority (scope syntax in uapi `docs/tokens.md`); the server adds none of its own.
5. **One tool call, one request.** A uapi write is a transaction (stage, validate, commit, reload) in a single HTTP request, so no tool spans several requests and none needs client-side batching.

## Tool design

- One tool per curated endpoint, named after it: `network_interfaces_list`, `network_rule_replace`, `firewall_rule_delete`. Action words map to verbs: list/read (GET), create (POST), adopt (POST on the adopt sub-resource), replace (PUT), merge (PATCH, RFC 7396 only: the tool never sends RFC 6902 JSON Patch), delete (DELETE).
- Collection tools keep the plural resource name, item tools take the singular (`network_interfaces_list` against `network_interface_replace`). The 3.3.0 spec yields 266 tools; the count lives in the generator tests and moves only with the vendored spec.
- Full read and write surface from the first release, same parity as the provider. `/raw` is excluded: the provider forbids it, and an agent gains nothing from uci field names that no release promises to keep. The catalog is the spec, minus `/raw`.
- Tool inputs: path parameters and user-facing query parameters (`managed`, `validate`) are top-level arguments; a request body is the whole input when the endpoint has no path parameters, and a `body` object beside the id otherwise. ETag preconditions (`if_match`, `if_none_match`) are deliberately not exposed: reads do not surface ETags, so an agent has nothing to guard with, and an interactive agent re-reads anyway.
- Annotations tell the truth: `readOnlyHint` on reads, `destructiveHint` on delete and replace, `idempotentHint` where the verb is. PUT with `id` is replace; the response says what uci holds now. `openWorldHint` is false: the domain of every tool is this one router.
- Errors carry the endpoint, the HTTP status, and uapi's error envelope (`code` + `message`, `docs/errors.md`) verbatim. Retry 423 locked and 429 within a wall-clock budget as the provider does, then surface the last status. An agent that reads the error must learn what to do next, not that something failed.
- Reads return the JSON payload as received. The host and the agent format; the server does not render markdown. Writes return the payload as their own content block, so it stays valid JSON for hosts, and append the transaction headers uapi sent (`X-Mgmt-Path-Warning`, `X-Reload-Status`, `X-Reload-Services`, `X-Kernel-Status`, `X-Kernel-Applied`) as a second block, because they say what the transaction did beyond committing uci. A DELETE that answers 404 is success: the section is already absent, and the result says so.

### Generation

The catalog is generated, never hand-edited: `internal/gen` reads the vendored spec (`internal/gen/openapi.json`, copied from uapi's `build/openapi.json`) and writes `internal/tools/catalog.json`, embedded with `go:embed` and wired by `internal/tools`. `make gen` regenerates; CI fails when the committed catalog is stale.

Input schemas are the spec's request schemas, then softened, so the client never rejects a payload the server accepts:

- enums fold into property descriptions (`One of (may grow): ...`), never into validation
- patterns, bounds (`minimum`, `maxLength`, ...) and format keywords are dropped
- `allOf` conditionals are dropped; the field descriptions carry what they taught
- every object accepts unknown properties, at every depth
- types, descriptions, `required` and `deprecated` stay
- `<Name>Patch` bodies take their field descriptions from `<Name>Request`, which the spec leaves out to save bytes on the router

A uapi minor can add enum values, fields and query parameters, and loosen bounds: all of that passes through the softened schemas without a uapi-mcp release. The generator tests hold the line: no forbidden keyword survives anywhere in any input schema.

## Version numbering

Mirrors uapi the way the provider does: `x.y` tracks the uapi `x.y` wire surface, the patch line is our own. A uapi minor with new resources or fields maps to a uapi-mcp minor (bump the vendored spec, `make gen`, update the endpoint count in the generator tests); a uapi major maps to a major.

## Family pointers

- `~/repo/uapi`: the server, in ucode. `docs/resources.md` is the endpoint catalog, `docs/versioning.md` the stability contract, `docs/errors.md` the error shapes, `docs/tokens.md` the auth model. Its CLAUDE.md carries the org-wide workflow rules.
- `~/repo/terraform-provider-uapi`: the reference Go client. `internal/client` owns bearer auth, 423/429 retry, idempotency keys, cursor pagination and error decoding; uapi's served spec (`/api/v3/openapi.json`, vendored in the repo at `build/openapi.json`) drives its generated schemas. Reuse the patterns, ignore the tfsdk layer.
- SDK: `github.com/modelcontextprotocol/go-sdk`, the official Tier 1 Go SDK, package `mcp`.

## House style (openwrt-iac, org-wide)

- Priorities, in order: simplicity, maintainability, modularity, readability. No over-engineering, no defensive code for cases the rest of the code makes unreachable.
- All changes land via a PR, never a direct push to `main`. Branches: `feat/<topic>`, `fix/<topic>`, `release/v<version>`. One-line commit messages.
- No em-dashes, in code, comments, docs or commits. Comments are rare; when one exists it explains a non-obvious why, never the what.
- Every PR adds its `CHANGELOG.md` entry under `## [Unreleased]`, ending with `(#<PR>)`.

## Testing

Three layers, each one watching a different thing:

- Unit tests against `httptest` stubs, for the client and the tool layer: they assert handler and client behavior in isolation.
- Host-eye tests in `internal/server`: an SDK client over the in-memory transport asserts what a host receives on the wire. Both defects of the first live run (an empty collection answering `null`, the transaction footer glued to the payload) were invisible from the inside and obvious from the host side, so result shapes live here, not in the handler tests: a result that carries a payload keeps its first content block valid JSON, a write with transaction headers carries the footer as a second block, and every list path has its empty-collection case.
- `make smoke` (scripts/smoke.py) drives the built binary over stdio against a real box running a real uapi; it is the gate before every tag. Anything that asserts what the router actually did needs this run, and the PR says so plainly.
