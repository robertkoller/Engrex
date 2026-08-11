package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/robertkoller/engrex/internal/protocol"
)

const defaultTimeout = 90 * time.Second

type daemonClient struct {
	socketPath string
	timeout    time.Duration
}

func newDaemonClient() (*daemonClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &daemonClient{
		socketPath: filepath.Join(home, ".engrex", "daemon.sock"),
		timeout:    defaultTimeout,
	}, nil
}

type daemonError struct {
	Code    string
	Message string
}

func (err *daemonError) Error() string { return err.Message }

func (client *daemonClient) send(ctx context.Context, command protocol.Command) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		return nil, &daemonError{
			Code:    protocol.CodeDaemonUnavailable,
			Message: "the Engrex daemon is not running — start it with `engrex daemon`",
		}
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, &daemonError{Code: protocol.CodeInternalError, Message: err.Error()}
		}
	}

	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return nil, &daemonError{
			Code:    protocol.CodeDaemonUnavailable,
			Message: "failed to send the request to the daemon: " + err.Error(),
		}
	}

	var response protocol.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return nil, &daemonError{
			Code:    protocol.CodeDaemonUnavailable,
			Message: "the daemon closed the connection without answering: " + err.Error(),
		}
	}
	if response.Error != "" {
		code := response.Code
		if code == "" {
			code = protocol.CodeInternalError
		}
		return nil, &daemonError{Code: code, Message: response.Error}
	}
	return response.Data, nil
}

func call[Payload any](ctx context.Context, client *daemonClient, command protocol.Command) (Payload, error) {
	var payload Payload

	data, err := client.send(ctx, command)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, &daemonError{
			Code:    protocol.CodeInternalError,
			Message: "could not decode the daemon's response: " + err.Error(),
		}
	}
	return payload, nil
}
