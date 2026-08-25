# muro — Security & Correctness Review

Conducted 2026-08-25, after the v1 feature set (SPEC.md → DESIGN.md →
IMPLEMENTATION.md) was fully implemented, committed, and independently
verified end to end. This review reads the actual code on disk directly —
not the design docs — with an attacker's mindset calibrated to SPEC.md §3's
threat model: a single-operator machine, a well-behaved but unpredictable AI
agent to be kept "inside its lane" by the OS, not a hardened boundary
against a maliciously adversarial workload.

**Coverage**: full line-by-line review of `internal/sandbox` (+
`cmd/muro-shim`) and `internal/proxy`. Targeted review of `internal/control`,
`internal/cli`, `internal/config`, `internal/state` focused on the specific
risk categories below — not an exhaustive line-by-line pass on those four.

No critical or high-severity findings. Six medium findings, all concrete and
fixable, listed below with file references. A longer list of lower-severity/
informational items and verified-sound conclusions follows.

## Medium findings

### 1. No mount host-path validation
**File**: `internal/config/validate.go`

`ValidateProfile` only checks `restart_policy` validity and tools/mounts
path collisions. Nothing stops a profile from mounting `/`, `/etc`, `/home`,
or muro's own state directory as read-write, and nothing stops a mount's
`SandboxPath` from landing on `/proc`, `/dev`, or `/tmp` — overriding
bwrap's own restricted scaffolding for those paths.

Not exploitable by the sandboxed *agent* itself (mounts are fixed at
launch; `Isolator.UpdateMounts` never allows a live remount). Real gap
against "give an agent enough access and nothing else," especially since
DESIGN.md explicitly expects profiles to often be tooling-generated rather
than hand-reviewed.

### 2. Path traversal via unsanitized namespace/name
**File**: `internal/config/paths.go:55` (`SandboxLogPath`)

```go
return filepath.Join(stateDir, "logs", "sandbox", namespace+"__"+name+".log"), nil
```

Builds the filename by direct string concatenation before `filepath.Join`.
A namespace or name containing `../` sequences escapes the intended
`logs/sandbox/` directory — `filepath.Join` cleans the *final* path but
does not stop one component's traversal sequences from escaping the base.

These values are always CLI-flag-provided by whoever runs `muro run` —
given the single-operator threat model, this is currently self-inflictable
rather than remotely exploitable. Still a real, concrete gap that should be
closed at the input boundary (reject unsafe characters in `--name`/
`--namespace` at CLI parsing or control-API dispatch time) rather than
relying on every path-consuming function downstream to individually guard
against it.

### 3. No timeouts on the proxy's `http.Server`
**File**: `internal/proxy/proxy.go:113`

```go
srv := &http.Server{Handler: http.HandlerFunc(s.handle)}
```

No `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`/`IdleTimeout` anywhere
in the request path (the only timeout anywhere is `peekAndCrossCheckSNI`'s
3s deadline, which only applies after a CONNECT has already been hijacked).
A process inside *any* bridged sandbox can open many slow/idle connections
to exhaust the shared proxy's goroutines/fds — since one `murod` process's
proxy serves every sandbox it manages, one misbehaving sandbox can deny
network access to every other co-located sandbox. Cheap, standard fix
(Go's classic slow-loris mitigation).

### 4. TOCTOU window in `applyEgressRestriction`'s error handling
**File**: `internal/sandbox/network.go`

Between `InnerNamespacePID` finding the sandbox's inner-namespace PID and
`nsenter`/`nft` actually running against it, the original process could
exit and that PID be reused by an unrelated process. The existing code
re-checks `isAlivePID` only *after* a command error — not before a
"successful" run that happened to target a reused PID. Worst case is
collateral (a firewall rule silently applied to a random unrelated
process), not a sandbox-isolation bypass — if the original process already
exited, there was nothing left needing restriction. Narrow, low-likelihood,
worth a defensive re-check regardless.

### 5. Zero test coverage for the SNI-mismatch deny path
**File**: `internal/proxy/proxy.go` (`peekAndCrossCheckSNI`'s deny branch),
`internal/proxy/proxy_test.go`

The domain-fronting defense (CONNECT target allowed, but the TLS SNI
presented afterward doesn't match) has no regression test anywhere —
`TestHandleCONNECT_Denied`/`TestHandleCONNECT_Allowed` only exercise the
CONNECT-target-level check. A future refactor could silently break this
defense with nothing to catch it.

### 6. Inconsistent directory permissions in `internal/state/store.go`

`persist()`'s own `os.MkdirAll(dir, 0o755)` is looser than
`cmd/murod/main.go`'s explicit `os.MkdirAll(stateDir, 0o700)` for the same
directory. Currently masked — murod always creates the directory first, and
`MkdirAll` is a no-op against an already-existing directory — but latent: a
future caller path could create `~/.local/state/muro/` world-readable,
exposing `state.json`'s mount paths and sandbox metadata to other local
users.

## Lower-severity / informational

These were checked and found not currently exploitable, given an existing
mitigation — listed for completeness and because the mitigation is
incidental (a parent directory permission), not a deliberate defense at the
point in question, so it's worth knowing about if that incidental
protection ever changes.

- **Shim attach socket has no explicit `chmod`** (`cmd/muro-shim/main.go`,
  `net.Listen("unix", spec.SocketPath)`) — but its parent directory is
  `0700` (both `BwrapIsolator.Launch`'s `runDir` and the shim's own
  `MkdirAll` before listening), blocking any other local user from
  traversing to it at all.
- **Control socket has a real bind-then-`chmod` TOCTOU window**
  (`internal/control/server.go:60-64` — `net.Listen` then `os.Chmod`) —
  same mitigation: `~/.local/state/muro/` is `0700`.
- **SNI domain-fronting degrade path** (proxy) narrows to "a shared
  front-end at an already-allowed host routing to a different backend,"
  not a real destination bypass — the dial target is fixed by the
  allowlist-checked CONNECT line before the SNI check ever runs.
- **`$EDITOR` with embedded spaces** (`internal/cli/profile.go`) fails to
  launch (Go's `exec.Command` never goes through a shell) rather than being
  exploitable — a usability quirk, not a vulnerability.
- Proxy denial-log attribution keys on an empty string when a sandbox key
  can't be resolved — cosmetic log-attribution gap, does not affect
  enforcement, previously known.

## Verified sound

Checked and found correct — worth stating explicitly, several of these
were expected to have gaps going in and didn't:

- **nftables egress ruleset is airtight**: `table inet` + `policy drop`
  correctly fails closed across IPv4, IPv6, UDP, and ICMP, not just the one
  explicit TCP-to-proxy accept rule.
- **No command injection anywhere** in bwrap argument construction or CLI
  `exec.Command` usage — every call site uses argv-array form, never
  shell-string interpolation.
- **`internal/proxy/sni.go`'s hand-rolled TLS ClientHello parser is
  bounds-safe** — every slice operation traced against its preceding length
  guard; this is exactly the kind of hand-parsed-attacker-controlled-bytes
  code that commonly has these bugs, and it doesn't here.
- **Allowlist matching has no bypass** — userinfo tricks, casing, port
  confusion, and wildcard/substring matching were all checked and closed.
- **Fail-closed is consistently enforced** in the proxy's request handling
  — every error/unresolved-sandbox-key path denies, none accidentally
  allows through.
- **`--uid 0 --gid 0` (bwrap.go) genuinely stays unprivileged on the host**
  (confirmed via `/proc/<pid>/uid_map`'s `0 <host-uid> 1` mapping) — the
  same safe pattern rootless Podman/Docker use.
- **Attach exclusivity is correctly enforced at the `Manager` level**
  (`attachRegistry.TryAttach`), before a second `muro sandbox attach` ever
  reaches the shim.
