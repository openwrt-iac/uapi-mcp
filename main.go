// Command uapi-mcp is a local stdio MCP server that exposes one OpenWrt
// router's uapi REST API to an agent session. The MCP host spawns it and owns
// its lifecycle; credentials come from the environment with the same names
// and semantics as the Terraform provider: UAPI_ENDPOINT, UAPI_TOKEN and
// UAPI_INSECURE.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openwrt-iac/uapi-mcp/internal/client"
	"github.com/openwrt-iac/uapi-mcp/internal/tools"
)

var version = "dev"

const instructions = `This server drives one OpenWrt router through its uapi REST API. Every tool is one HTTP request to one curated endpoint, named after it: domain, resource, verb (list, read, create, replace, merge, delete, adopt). Reads return the JSON payload as received. Writes are atomic transactions: the response says what uci holds now, and a footer carries the reload and kernel status headers when uapi sent them. Errors name the endpoint, the HTTP status and uapi's error envelope, with field-level detail for 422. Read a resource before writing it; on a firewall rule for example, call firewall_rule_read, edit the object, and send it back as the body of firewall_rule_replace.`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "uapi-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := strings.TrimSpace(os.Getenv("UAPI_ENDPOINT"))
	token := strings.TrimSpace(os.Getenv("UAPI_TOKEN"))
	if endpoint == "" {
		return errors.New("UAPI_ENDPOINT is not set: it must hold the router's uapi base URL, for example https://192.168.1.1/api/v3")
	}
	if token == "" {
		return errors.New("UAPI_TOKEN is not set: mint one on the router with uapi-token create")
	}
	insecure := func() bool {
		v := os.Getenv("UAPI_INSECURE")
		return v == "1" || v == "true"
	}()

	c := client.New(endpoint, token, client.UserAgent(version), insecure)
	s := mcp.NewServer(&mcp.Implementation{Name: "uapi-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	if err := tools.Register(s, c); err != nil {
		return err
	}
	return s.Run(context.Background(), &mcp.StdioTransport{})
}
