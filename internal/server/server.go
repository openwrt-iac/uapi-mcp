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

const instructions = `This server drives one OpenWrt router through its uapi REST API. Every tool is one HTTP request to one curated endpoint, named after it: domain, resource, verb (list, read, create, replace, merge, delete, adopt). Reads return the JSON payload as received. Writes are atomic transactions: the response says what uci holds now, and a footer carries the reload and kernel status headers when uapi sent them. Errors name the endpoint, the HTTP status and uapi's error envelope, with field-level detail for 422. Read a resource before writing it; on a firewall rule for example, call firewall_rule_read, edit the object, and send it back as the body of firewall_rule_replace.`

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
