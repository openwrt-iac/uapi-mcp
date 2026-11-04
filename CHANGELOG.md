# Changelog

All notable changes are documented here. uapi-mcp mirrors uapi: version `x.y.*`
covers the curated surface of uapi `x.y.*` (patch is uapi-mcp's own line). Format
follows Keep a Changelog.

## [Unreleased]

### Added
- A host-eye test layer in `internal/server`: an SDK client over the in-memory
  transport asserts what a host receives, the layer that would have caught both
  defects of the first live run before any release. `main.go` now only owns the
  process; `internal/server.New` assembles the server. Added `make smoke`
  (`scripts/smoke.py`), the one-command run against a real router that gates
  every tag, and wrote the three testing layers down in AGENTS.md. (#5)

### Fixed
- Write results glued the transaction-header footer to the payload text, so
  hosts could not parse them as JSON: writes answered a raw string while reads
  answered an object. The payload now rides as its own content block and the
  footer follows as a second block, byte for byte unchanged otherwise. (#4)

## [3.3.0] - 2026-11-04

First release, tracking uapi 3.3.0.

### Added
- Initial project: a stdio MCP server that exposes one OpenWrt router's uapi REST
  API to an agent session. Credentials come from `UAPI_ENDPOINT`, `UAPI_TOKEN` and
  `UAPI_INSECURE`, same names and semantics as the Terraform provider. The tool
  catalog is generated from uapi's vendored OpenAPI spec (3.3.0) by
  `internal/gen`: 266 tools, one per curated endpoint, /raw excluded. Input
  schemas are the spec's request schemas softened so the client never rejects a
  payload the server accepts: enums fold into property descriptions, patterns and
  bounds are dropped, objects accept unknown properties. (#2)
- CI in the provider's shape (build, vet, gofmt, race tests, golangci-lint,
  catalog freshness) plus uapi's changelog entry check, and a GoReleaser release
  workflow: a signed `v*` tag, verified against `.github/release-signers.asc`,
  builds multi-platform binaries and publishes them with notes extracted from
  this changelog. The artifacts are not signed; the signed tag is the trust
  anchor. (#2)

### Fixed
- An empty collection answered `null` instead of `[]`: the list tools
  marshalled a nil slice. Found on a real router, where an empty collection is
  the normal case, such as the WireGuard kernel peers on a router without a
  WireGuard interface. (#3)
