// Package server assembles the uapi-mcp MCP server: implementation
// metadata, instructions, and the generated tool catalog wired to one
// router. main owns the process; this owns the server, so tests can drive
// the full stack the way an MCP host does.
package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openwrt-iac/uapi-mcp/internal/client"
	"github.com/openwrt-iac/uapi-mcp/internal/tools"
)

const instructions = `This server drives one OpenWrt router through its uapi REST API. Every tool is one HTTP request to one curated endpoint, named after it: domain, resource, verb (list, read, create, replace, merge, delete, adopt, recover). Reads return the JSON payload as received. Writes are atomic transactions: the response says what uci holds now, and a footer carries the reload and kernel status headers when uapi sent them. X-Reload-Status: stopped means that uci holds the change but procd gave up on a daemon, and X-Reload-Stopped names each such instance as <service>/<instance>: call uapi_service_recover once for each service that it names, and if that answers an error, a 403 included, tell the user which instances stay down. X-Kernel-Status: blocked means that uci holds the change but a stored wireguard peer keeps its interface down, and X-Kernel-Blocked names each such peer as <interface>/<peer id>: read the errors with uapi_diagnostics_read and validate "1", and ask the user before you change a peer that the task does not name. Tell the user about any other footer header. A wireguard kernel peer read carries X-Kernel-Unread in the same footer: the payload holds no peer for the interfaces it names, so an empty list beside it is not a kernel without drift. Errors name the endpoint, the HTTP status and uapi's error envelope, with field-level detail for 422. Read a resource before writing it. To change some fields, send only those fields to the merge tool, such as firewall_rule_merge: a merge keeps the options that uapi does not model. A replace, such as firewall_rule_replace, sets the whole section and removes such options, such as the advanced options that LuCI writes on an sqm queue, so use it only when the user wants the whole section replaced.`

// New builds the server for one router. The catalog registers every
// curated uapi endpoint; Register fails only on a corrupt embedded catalog.
func New(version string, c *client.Client) (*mcp.Server, error) {
	s := mcp.NewServer(&mcp.Implementation{Name: "uapi-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	if err := tools.Register(s, c); err != nil {
		return nil, err
	}
	return s, nil
}
