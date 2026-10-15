package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/compscidr/sair/internal/agentapi"
	"github.com/compscidr/sair/internal/agentmcp"
	"github.com/compscidr/sair/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const usage = `Usage: sair-mcp [--version | --help]

An MCP server (stdio) that lets AI agents run Android tests on real phones
through the SAIR agent API. Takes no arguments; it is configured through env vars:

  SAIR_API_KEY  API key from https://sair.run/dashboard (required)
  SAIR_API_URL  Agent API base URL (default: https://api.sair.run)
`

func main() {
	version.HandleArgs(usage)
	key := os.Getenv("SAIR_API_KEY")
	if key == "" {
		fmt.Fprint(os.Stderr, "SAIR_API_KEY is not set\n\n"+usage)
		os.Exit(2)
	}
	url := os.Getenv("SAIR_API_URL")
	if url == "" {
		url = "https://api.sair.run"
	}
	s := agentmcp.NewServer(&agentapi.Client{BaseURL: url, APIKey: key}, version.Version)
	// stdout is the protocol; logs go to stderr.
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
