package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/robertkoller/engrex/internal/config"
	"github.com/robertkoller/engrex/internal/mcpserver"
	"github.com/spf13/cobra"
)

func mcpCommand() *cobra.Command {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Expose the knowledge base to MCP clients (Claude Desktop, etc.)",
		Long: "Manage the Model Context Protocol interface. MCP clients get three read-only tools — " +
			"search_notes, get_document, and query_knowledge_graph — served over stdio and backed by " +
			"the running daemon. Nothing is bound to a network port and nothing leaves the machine.",
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server on stdio (spawned by the MCP client, not by hand)",
		Long: "Speaks MCP over stdin/stdout and forwards every tool call to the running daemon. This is " +
			"what you point an MCP client's command at; running it in a terminal just waits for JSON-RPC " +
			"frames on stdin. Requires `engrex daemon` to be running and MCP to be enabled.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			if err := mcpserver.Serve(ctx); err != nil {

				fmt.Fprintf(os.Stderr, "engrex mcp: %v\n", err)
				return err
			}
			return nil
		},
	}

	enableCmd := &cobra.Command{
		Use:   "enable",
		Short: "Allow MCP clients to read the knowledge base",
		Long: "Turns the MCP interface on in ~/.engrex/config.json. It is off by default: MCP is local-only, " +
			"but it is still another way into your notes, so it has to be opted into. Takes effect on the " +
			"next MCP request — no daemon restart needed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setMCPEnabled(true)
		},
	}

	disableCmd := &cobra.Command{
		Use:   "disable",
		Short: "Stop MCP clients from reading the knowledge base",
		Long: "Turns the MCP interface off. The daemon will refuse MCP tool calls from then on, without " +
			"affecting the CLI, the browser extension, or the file watcher.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setMCPEnabled(false)
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the MCP interface is enabled",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configuration, err := config.Load()
			if err != nil {
				return err
			}
			path, err := config.Path()
			if err != nil {
				return err
			}
			state := "disabled"
			if configuration.MCPEnabled {
				state = "enabled"
			}
			fmt.Printf("MCP interface: %s\nConfig: %s\n", state, path)
			return nil
		},
	}

	mcpCmd.AddCommand(serveCmd)
	mcpCmd.AddCommand(enableCmd)
	mcpCmd.AddCommand(disableCmd)
	mcpCmd.AddCommand(statusCmd)
	return mcpCmd
}

func setMCPEnabled(enabled bool) error {
	configuration, err := config.Load()
	if err != nil {
		return err
	}
	configuration.MCPEnabled = enabled
	if err := config.Save(configuration); err != nil {
		return err
	}

	if enabled {
		fmt.Println("MCP interface enabled. Point your MCP client at: engrex mcp serve")
	} else {
		fmt.Println("MCP interface disabled.")
	}
	return nil
}
