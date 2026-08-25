package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Client is a control API client — used by cmd/muro, and by any future
// client of the same API (DESIGN.md §5: "a future TUI/web dashboard would
// be just another client of the same control API"). Client owns its
// connection for its whole lifetime via a single bufio.Reader, exactly
// mirroring how Server.handleConn owns its side, so the sandbox.attach
// stream upgrade (Attach, below) never loses bytes the reader may have
// already buffered ahead of a response's newline.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
}

// Dial connects to murod's control socket at socketPath.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial control socket: %w", err)
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Call sends one request of type reqType with payload marshaled as its
// JSON body, reads the single-line Response, and — if the response is
// OK and out is non-nil — unmarshals the response payload into out.
// payload may be nil for request types with no fields (e.g.
// broker.status). Returns an error (from Response.Error) if the server
// reported OK:false.
func (c *Client) Call(reqType string, payload any, out any) error {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal request payload: %w", err)
		}
		raw = data
	}

	req := Request{Type: reqType, Payload: raw}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.conn.Write(data); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("malformed response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if out != nil && len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, out); err != nil {
			return fmt.Errorf("unmarshal response payload: %w", err)
		}
	}
	return nil
}

// Attach sends a sandbox.attach request and, once the server's JSON
// handshake confirms OK, returns the connection as a raw io.Reader/
// io.Writer pair ready for bidirectional byte passthrough (server.go/
// stream.go — the pty of the target sandbox). The returned io.Reader is
// Client's own bufio.Reader, not the raw conn, so any bytes already
// buffered ahead of the handshake response's newline are still delivered
// rather than silently dropped. The caller (cmd/muro, a later task) is
// responsible for wiring these to the local terminal (raw mode, resize
// forwarding, watching for sandbox.DetachSequence itself if it wants to
// stop without closing the connection) — Client does not interpret the
// stream's contents at all once the handshake succeeds.
func (c *Client) Attach(namespace, name string) (io.Reader, io.Writer, error) {
	req := Request{
		Type:    TypeSandboxAttach,
		Payload: mustMarshal(SandboxAttachRequest{Namespace: namespace, Name: name}),
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal attach request: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.conn.Write(data); err != nil {
		return nil, nil, fmt.Errorf("write attach request: %w", err)
	}

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("read attach handshake: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, nil, fmt.Errorf("malformed attach handshake: %w", err)
	}
	if !resp.OK {
		return nil, nil, fmt.Errorf("%s", resp.Error)
	}

	return c.r, c.conn, nil
}

// Logs sends a `logs` request and, once the server's JSON handshake
// confirms OK, returns the connection as a raw io.Reader ready to be
// copied to the caller's own stdout — the sandbox's captured output
// (server.go/stream.go), starting with whatever already existed and (if
// follow is true) continuing with newly-appended content until the server
// closes the connection or the caller stops reading. One-directional,
// unlike Attach — nothing the caller writes is ever read by the server for
// this request type, so Logs deliberately returns only a Reader.
func (c *Client) Logs(namespace, name string, follow bool) (io.Reader, error) {
	req := Request{
		Type:    TypeLogs,
		Payload: mustMarshal(LogsRequest{Namespace: namespace, Name: name, Follow: follow}),
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal logs request: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write logs request: %w", err)
	}

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read logs handshake: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("malformed logs handshake: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}

	return c.r, nil
}
