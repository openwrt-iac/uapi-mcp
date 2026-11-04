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
	"github.com/openwrt-iac/uapi-mcp/internal/server"
)

var version = "dev"

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
	s, err := server.New(version, c)
	if err != nil {
		return err
	}
	return s.Run(context.Background(), &mcp.StdioTransport{})
}
