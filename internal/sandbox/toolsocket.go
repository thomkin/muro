package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/gitproxy"
)

// ToolExecRequest is the one message shape a sandbox's git tool-proxy stub
// (cmd/muro-toolstub) sends, newline-terminated JSON — the same framing
// convention as agentsocket.go's AgentPublishRequest, on a separate socket
// (ToolSocketMountPath) so the two protocols never need to share a shape.
type ToolExecRequest struct {
	Tool string   `json:"tool"` // only "git" is supported in v1
	Argv []string `json:"argv"` // does NOT include the tool name itself
	Cwd  string   `json:"cwd"`  // sandbox-side cwd the stub was invoked from
}

type ToolExecResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// maxToolRequestSize bounds a single tool-socket request — generous for any
// realistic git invocation's argv, same reasoning as agentsocket.go's
// maxAgentRequestSize.
const maxToolRequestSize = 256 << 10

// toolSocketServer is the per-sandbox listener for the git tool-proxy —
// started by Manager (startToolBridge) BEFORE Launch, using the same
// host-side path (ToolSocketPath, bwrap.go) BwrapIsolator mounts into the
// sandbox, so the listener is always up before the sandboxed process could
// possibly connect (same reasoning as agentSocketServer).
type toolSocketServer struct {
	mounts                   []config.Mount
	gitPolicy                config.GitPolicy
	daemonAllowedSubcommands []string
	path                     string
	listener                 net.Listener
}

// startToolSocket listens on path (creating its parent directory and
// setting 0600 permissions, matching startAgentSocket's setup) and starts
// serving.
func startToolSocket(path string, mounts []config.Mount, gitPolicy config.GitPolicy, daemonAllowedSubcommands []string) (*toolSocketServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create tool socket dir: %w", err)
	}
	os.Remove(path) // stale socket from a previous run at this same path, if any
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on tool socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod tool socket: %w", err)
	}
	s := &toolSocketServer{
		mounts:                   mounts,
		gitPolicy:                gitPolicy,
		daemonAllowedSubcommands: daemonAllowedSubcommands,
		path:                     path,
		listener:                 ln,
	}
	go s.acceptLoop()
	return s, nil
}

func (s *toolSocketServer) stop() {
	_ = s.listener.Close()
	_ = os.Remove(s.path)
}

func (s *toolSocketServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed (sandbox stopping) or a real error either way — stop accepting
		}
		go s.handleConn(conn)
	}
}

// toolExecTimeout bounds how long a single git invocation (including its
// dry-run pre-flight for push) is allowed to run — generous for any
// realistic git operation over a local or normal-latency remote, while
// still guaranteeing handleConn can't block forever on a hung network
// operation.
const toolExecTimeout = 60 * time.Second

func (s *toolSocketServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(conn)
	data, err := readLineLimitedBytes(r, maxToolRequestSize)
	if err != nil {
		writeToolResponse(conn, ToolExecResponse{Error: fmt.Sprintf("read request: %v", err)})
		return
	}

	var req ToolExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		writeToolResponse(conn, ToolExecResponse{Error: fmt.Sprintf("malformed request: %v", err)})
		return
	}

	if req.Tool != "git" {
		writeToolResponse(conn, ToolExecResponse{Error: fmt.Sprintf("unsupported tool %q", req.Tool)})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), toolExecTimeout)
	defer cancel()
	result := gitproxy.Handle(ctx, gitproxy.Request{Argv: req.Argv, Cwd: req.Cwd}, s.mounts, s.gitPolicy, s.daemonAllowedSubcommands)

	// A fresh write deadline: the read deadline above may already be close
	// to expiring by the time a real git invocation (push, in particular)
	// finishes.
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	writeToolResponse(conn, ToolExecResponse{
		OK:       result.OK,
		Error:    result.Error,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		ExitCode: result.ExitCode,
	})
}

func writeToolResponse(conn net.Conn, resp ToolExecResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = conn.Write(data)
}
