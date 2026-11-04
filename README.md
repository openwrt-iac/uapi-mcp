# uapi-mcp

An MCP server (stdio) that exposes [uapi](https://github.com/openwrt-iac/uapi), the native
HTTP REST API for OpenWrt, to an agent session. One tool per curated endpoint, one HTTP
request per tool call, atomic writes, errors that say what to do next. It is the third
member of the openwrt-iac family, beside the API itself and the Terraform provider.

> Not affiliated with the OpenWrt project. This server started as the work of a single
> operator solving a specific problem on their own network, shared in the open in the
> hope it is useful to others with similar needs.

## Requirements

- An OpenWrt router running uapi >= 3.0.0 (OpenWrt 25.12+), reachable over HTTP(S). The
  tool catalog is generated from uapi's 3.3.0 spec; tools for endpoints an older router
  does not serve answer 404, and fields an older router does not know answer 422
  `unknown_field`.
- A bearer token created on the router: `uapi-token create --name agent --scope '*:rw'`
  (narrow the scopes to what the agent should touch).
- An MCP host that spawns stdio servers: OpenCode, Claude Code, gh-aw, and others.
- Go >= 1.27 to build.

## Build and configuration

```sh
make build   # produces ./uapi-mcp
```

The server is a local stdio process and reads no configuration but three environment
variables, same names and semantics as the Terraform provider:

| Variable        | Meaning                                                        |
|-----------------|----------------------------------------------------------------|
| `UAPI_ENDPOINT` | API root including the version prefix, for example `https://192.168.1.1/api/v3`. |
| `UAPI_TOKEN`    | Bearer token minted with `uapi-token create`.                  |
| `UAPI_INSECURE` | `1` or `true` to skip TLS verification, needed for uapi's default self-signed certificate. |

A missing `UAPI_ENDPOINT` or `UAPI_TOKEN` fails the process at startup with a message on
stderr; the MCP host surfaces it.

Example for an OpenCode client configuration:

```jsonc
{
  "mcp": {
    "uapi": {
      "type": "local",
      "command": ["/usr/local/bin/uapi-mcp"],
      "environment": {
        "UAPI_ENDPOINT": "https://192.168.1.1/api/v3",
        "UAPI_TOKEN": "…"
      }
    }
  }
}
```

## The tool surface

The catalog is generated from uapi's OpenAPI spec, vendored at `internal/gen/openapi.json`
and regenerated with `make gen`. The 3.3.0 spec yields 266 tools, one per endpoint, the
`/raw/` passthrough excluded: raw payloads follow uci's field names and carry no
stability promise across OpenWrt releases, so they are not a fit for an agent either.

Tools are named after their endpoint: domain, resource, verb. Collection tools keep the
plural (`network_interfaces_list`, `firewall_rules_create`), item tools take the singular
(`network_rule_replace`, `firewall_rule_delete`). Verbs follow the HTTP method:

| Verb      | Method | Notes                                        |
|-----------|--------|----------------------------------------------|
| `list`    | GET    | Collection; the client follows cursor pagination and returns one array. |
| `read`    | GET    | One resource, or a singleton.               |
| `create`  | POST   | The tool arguments are the request body.     |
| `adopt`   | POST   | Takes a section id, for anonymous sections.  |
| `replace` | PUT    | Arguments: the id and a `body` object.      |
| `merge`   | PATCH  | RFC 7396 merge-patch; a `null` removes a field. |
| `delete`  | DELETE | A 404 is success: the section is already absent. |

Reads return the JSON payload exactly as the router sent it. Writes return what uci holds
now, plus a footer with uapi's transaction headers (`X-Reload-Status`,
`X-Kernel-Status`, and the management-path warning) when the router sent them. Errors
carry the endpoint, the HTTP status and uapi's error envelope verbatim, field by field
for 422, so the agent can fix everything in one round trip.

### Forward compatibility

The input schemas are generated from the spec's request schemas, then softened so this
client never rejects a payload the server accepts, the same discipline the provider
follows:

- Enums fold into property descriptions (`One of (may grow): …`), never into validation:
  uapi may add values within a major.
- Patterns and bounds are dropped: uapi may loosen them within a minor.
- Every object accepts unknown properties: uapi may add fields within a minor.
- Type corrections under uapi's documented carve-outs stay possible.
- Responses are passed through as received, never decoded into strict structs.
- The client branches on HTTP status only, and never hardcodes the API major: the
  version lives in `UAPI_ENDPOINT`.

`GET /schema/<package>/<resource>` tools give the agent the server's own view of a
resource, and `openapi_json_read` serves the whole document, when a description is not
enough.

## Version numbering

Mirrors uapi the way the provider does: `x.y` tracks the uapi `x.y` wire surface, the
patch line is our own. A uapi minor with new resources or fields maps to a uapi-mcp
minor (bump the vendored spec, run `make gen`, update the count in the generator tests);
a uapi major maps to a major.

## Development

```sh
make build   # build the binary
make test    # go test ./...
make gen     # regenerate internal/tools/catalog.json after a spec bump
make fmt     # gofmt
make vet     # go vet
```

Tests run against `httptest` stubs for the client and the tool layer. Anything that
asserts what a router actually did needs a run against a real box running a real uapi.

## Release

Releases are built by GoReleaser from a signed `v*` tag, `v3.3.0` for a release
tracking uapi 3.3.0. The tag must be signed by a key listed in
`.github/release-signers.asc`; the release workflow verifies it before building
anything. Binaries for linux, darwin, windows and freebsd (amd64, 386, arm,
arm64) and SHA256SUMS land on the GitHub Release, with notes extracted from
CHANGELOG.md. The artifacts themselves are not signed; the trust anchor is the
signed tag.

## License

MIT, see [LICENSE](LICENSE).
