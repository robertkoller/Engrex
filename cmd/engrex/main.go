package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/daemon"
	"github.com/robertkoller/engrex/internal/db"
	ragpkg "github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/store"
	"github.com/spf13/cobra"
)

type socketCommand struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

type socketResponse struct {
	Error string `json:"error,omitempty"`
}

// clearEngrexFolder deletes everything inside ~/Engrex (files and subfolders like
// RawText) while keeping the ~/Engrex directory itself so the watcher keeps working.
func clearEngrexFolder() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	engrexDir := filepath.Join(home, "Engrex")

	entries, err := os.ReadDir(engrexDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(engrexDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func sendCommand(command socketCommand) error {
	home, _ := os.UserHomeDir()
	socketPath := filepath.Join(home, ".engrex", "daemon.sock")

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return errors.New("daemon is not running — start it with `engrex daemon`")
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return err
	}

	if command.Type == "query" {
		reader := bufio.NewReader(conn)

		// First line is the JSON sources list; the rest is the answer stream.
		sourcesLine, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		var sourcesPayload struct {
			Sources []string `json:"sources"`
		}
		_ = json.Unmarshal([]byte(sourcesLine), &sourcesPayload)

		if _, err := io.Copy(os.Stdout, reader); err != nil {
			return err
		}

		if len(sourcesPayload.Sources) > 0 {
			fmt.Println("\nSources:")
			for _, source := range sourcesPayload.Sources {
				fmt.Printf("  - %s\n", filepath.Base(source))
			}
		}
		return nil
	}

	var response socketResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

// Entry point for the engrex CLI.
// Registers the `add` and `query` subcommands via cobra and delegates to rag.

func main() {
	database, err := db.Open()
	if err != nil {
		log.Fatal(err)
	}

	store := store.New(database)

	rag, err := ragpkg.New(store)
	if err != nil {
		log.Fatal(err)
	}
	cli := initializeCobra(rag, store)
	if err := cli.Execute(); err != nil {
		fmt.Print(err)
	}
}

// Gets all of the cli commands initialized
func initializeCobra(rag *ragpkg.RAG, store *store.Store) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "engrex",
		Short: "Your memory, on your machine",
		Long:  "Engrex is a local-first AI second brain. Save anything with `add`, ask anything with `query`. Everything stays on your machine — no cloud, no API keys.",
	}

	addCmd := &cobra.Command{
		Use:   "add [text]",
		Short: "Save text to your knowledge base",
		Long:  "Chunks, embeds, and stores the given text locally. Near-duplicate content is automatically skipped.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCommand(socketCommand{Type: "add", Text: args[0], Source: "cli"})
		},
	}
	queryCmd := &cobra.Command{
		Use:   "query [question]",
		Short: "Ask a question against your saved notes",
		Long:  "Embeds the question, finds the most relevant stored chunks, and streams an answer from a local LLM. If no relevant notes are found, answers from general knowledge and labels it clearly.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendCommand(socketCommand{Type: "query", Text: args[0]})
		},
	}
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "Show all stored chunks",
		Long:  "Prints every chunk in the database ordered by most recently saved, with its ID, timestamp, and text.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			chunks, err := store.List()
			if err != nil {
				return err
			}
			if len(chunks) == 0 {
				fmt.Println("No chunks stored yet.")
				return nil
			}
			for _, chunk := range chunks {
				var text string
				if len(chunk.Text) > 200 {
					text = chunk.Text[:200] + "..."
				} else {
					text = chunk.Text
				}
				fmt.Printf("[%d] %s | %s\n%s\n\n", chunk.ID, chunk.CreatedAt.Format("2006-01-02 15:04:05"), chunk.Source, text)
			}
			return nil
		},
	}

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete all stored chunks and files",
		Long:  "Wipes the entire knowledge base and everything in ~/Engrex after asking for confirmation. This cannot be undone.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print("This will delete all stored chunks AND every file in ~/Engrex. Are you sure? (y/n): ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(strings.ToLower(scanner.Text())) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}
			if err := store.Clear(); err != nil {
				return err
			}
			if err := clearEngrexFolder(); err != nil {
				return err
			}
			fmt.Println("Database and ~/Engrex cleared.")
			return nil
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete ?,?-?,...",
		Short: "Delete chosen chunks from store.",
		Long:  "Deletes specific chosen chunks from the knowledge base. This cannot be undone.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print("This will delete all chosen chunks. Are you sure? (y/n): ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(strings.ToLower(scanner.Text())) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}

			if err := sendCommand(socketCommand{Type: "delete", Text: strings.Join(args, "")}); err != nil {
				return err
			}

			fmt.Println("Chunks deleted successfully.")
			return nil
		},
	}

	debugCmd := &cobra.Command{
		Use:   "debug [question]",
		Short: "Show raw similarity distances for a query",
		Long:  "Embeds the question and prints every stored chunk with its exact distance score, bypassing all filters. Useful for tuning similarity thresholds.",
		RunE: func(cmd *cobra.Command, args []string) error {
			chunks, err := rag.DebugSearch(args[0])
			if err != nil {
				return err
			}
			if len(chunks) == 0 {
				fmt.Println("No chunks stored.")
				return nil
			}
			fmt.Printf("%-10s %-8s %s\n", "DISTANCE", "ID", "TEXT")
			fmt.Println(strings.Repeat("-", 80))
			for _, chunk := range chunks {
				text := chunk.Text
				if len(text) > 60 {
					text = text[:60] + "..."
				}
				fmt.Printf("%-10.4f %-8d %s\n", chunk.Distance, chunk.ID, text)
			}
			return nil
		},
	}

	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start the Engrex background daemon",
		Long:  "Starts the daemon which watches ~/Engrex/ for file saves and listens on a Unix socket for CLI commands. Runs until stopped.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			daemon, err := daemon.Start()
			if err != nil {
				return err
			}
			err = daemon.Run()
			return err
		},
	}

	rootCmd.AddCommand(addCmd)
	rootCmd.AddCommand(queryCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(clearCmd)
	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(daemonCmd)
	return rootCmd
}
