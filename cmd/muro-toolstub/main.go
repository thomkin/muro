// Command muro-toolstub is the sandbox-side half of muro's git tool-proxy:
// it is mounted into a sandbox at the git tool-proxy stub location
// (sandbox.GitStubMountPath) in place of a real git binary, and forwards
// every invocation to murod over the sandbox's tool socket
// (sandbox.ToolSocketMountPath, internal/sandbox/toolsocket.go), which
// validates it against the two-layer git policy and — if allowed — runs
// the real git on the host. v1 only ever sends Tool: "git"; a future
// multi-tool version would dispatch on argv[0] (the BusyBox-style pattern
// discussed but not needed while git is the only proxied tool).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/thomkin/muro/internal/sandbox"
)

func main() {
	os.Exit(run())
}

func run() int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: getwd: %v\n", err)
		return 1
	}

	conn, err := net.DialTimeout("unix", sandbox.ToolSocketMountPath, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: git tool-proxy unreachable at %s — is this sandbox configured with a git policy? %v\n", sandbox.ToolSocketMountPath, err)
		return 1
	}
	defer conn.Close()

	req := sandbox.ToolExecRequest{
		Tool: "git",
		Argv: os.Args[1:],
		Cwd:  cwd,
	}
	data, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: %v\n", err)
		return 1
	}
	data = append(data, '\n')

	_ = conn.SetDeadline(time.Now().Add(65 * time.Second)) // slightly above internal/sandbox/toolsocket.go's toolExecTimeout, so a legitimate slow git command isn't cut off by this side first
	if _, err := conn.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: write to tool socket: %v\n", err)
		return 1
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: read response from tool socket: %v\n", err)
		return 1
	}

	var resp sandbox.ToolExecResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "muro-toolstub: malformed response from tool socket: %v\n", err)
		return 1
	}

	if !resp.OK {
		fmt.Fprintln(os.Stderr, resp.Error)
		return 1
	}

	fmt.Fprint(os.Stdout, resp.Stdout)
	fmt.Fprint(os.Stderr, resp.Stderr)
	return resp.ExitCode
}
