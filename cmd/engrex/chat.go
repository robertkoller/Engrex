package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/protocol"
	"github.com/spf13/cobra"
)

// chatCommand is query with follow-ups. The conversation lives only in this process,
// so quitting ends it
func chatCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "chat",
		Short: "Ask questions with follow-ups against your saved notes",
		Long:  "Starts an interactive chat. Each answer can be followed up on, and follow-ups search your notes again. Type /new to start over, /exit or Ctrl-D to quit.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var history []protocol.Turn
			input := bufio.NewScanner(os.Stdin)
			fmt.Println("Engrex chat. /new starts over, /exit quits.")
			for {
				fmt.Print("\n> ")
				if !input.Scan() {
					fmt.Println()
					return input.Err()
				}
				question := strings.TrimSpace(input.Text())
				switch question {
				case "":
					continue
				case "/exit", "/quit":
					return nil
				case "/new":
					history = nil
					fmt.Println("Started a new conversation.")
					continue
				}

				answer, err := sendChatTurn(history, question)
				if err != nil {
					return err
				}
				history = append(history,
					protocol.Turn{Role: protocol.RoleUser, Content: question},
					protocol.Turn{Role: protocol.RoleAssistant, Content: answer},
				)
			}
		},
	}
}

// sendChatTurn sends one question with the history before it and streams the answer
func sendChatTurn(history []protocol.Turn, question string) (string, error) {
	home, _ := os.UserHomeDir()
	conn, err := net.Dial("unix", filepath.Join(home, ".engrex", "daemon.sock"))
	if err != nil {
		return "", errors.New("daemon is not running — start it with `engrex daemon`")
	}
	defer conn.Close()

	command := socketCommand{Type: "query", Text: question, History: history}
	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return "", err
	}
	return streamAnswer(conn)
}
