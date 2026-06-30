package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/robertkoller/engrex/internal/db"
	"github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/store"
	"github.com/spf13/cobra"
)

const defaultSearchDistance = 0.70
const defaultSearchResults = 10

// Entry point for the engrex CLI.
// Registers the `add` and `query` subcommands via cobra and delegates to rag.

func main() {
	database, err := db.Open()
	if err != nil {
		log.Fatal(err)
	}

	store := store.New(database)

	rag, err := rag.New(store)
	if err != nil {
		log.Fatal(err)
	}
	cli := initializeCobra(rag, store)
	if err := cli.Execute(); err != nil {
		fmt.Print(err)
	}
}

func initializeCobra(rag *rag.RAG, store *store.Store) *cobra.Command {
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
			return rag.Add(args[0], "cli")
		},
	}
	queryCmd := &cobra.Command{
		Use:   "query [question]",
		Short: "Ask a question against your saved notes",
		Long:  "Embeds the question, finds the most relevant stored chunks, and streams an answer from a local LLM. If no relevant notes are found, answers from general knowledge and labels it clearly.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return rag.Query(args[0], defaultSearchDistance, defaultSearchResults)
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
				fmt.Printf("[%d] %s | %s\n%s\n\n", chunk.ID, chunk.CreatedAt.Format("2006-01-02 15:04:05"), chunk.Source, chunk.Text)
			}
			return nil
		},
	}

	clearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete all stored chunks",
		Long:  "Wipes the entire knowledge base after asking for confirmation. This cannot be undone.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print("This will delete all stored chunks. Are you sure? (y/n): ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			if strings.TrimSpace(strings.ToLower(scanner.Text())) != "y" {
				fmt.Println("Cancelled.")
				return nil
			}
			if err := store.Clear(); err != nil {
				return err
			}
			fmt.Println("Database cleared.")
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

	rootCmd.AddCommand(addCmd)
	rootCmd.AddCommand(queryCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(clearCmd)
	rootCmd.AddCommand(debugCmd)
	return rootCmd
}
