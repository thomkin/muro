# muro — Design Specification

This document translates SPEC.md (the product/architecture spec) into concrete
implementation decisions: language, libraries, licenses, file layout,
packaging, and the exact CLI surface. Where SPEC.md left a decision open
(§10), this document either resolves it or explicitly restates that it's
still open and why.

## 1. Language & Toolchain

- **Language: Go.** Minimum version **Go 1.23**.
- Rationale: every implementation detail already sketched in SPEC.md assumes
  Go (`x/sys/unix` for namespaces, Go MQTT clients, "single static Go
  binary"). No polyglot components.
- Build: `CGO_ENABLED=0` for all four binaries — every chosen dependency
  (below) is pure Go, so static, fully self-contained binaries are the
  default build mode, not a stretch goal.
- Module layout: single Go module, single repo, four `cmd/` entrypoints
  (§4) — a fourth, `muro-shim`, was added during implementation; see §4.

## 2. Project License

- **muro is MIT-licensed.**
- All dependencies selected below are MIT, Apache-2.0, BSD, EPL-2.0, or
  MPL-2.0 — all permissive/weak-copyleft and compatible with shipping an
  MIT-licensed binary that statically links them. No GPL dependencies are
  used anywhere in the design.

## 3. Library Choices

| Concern | Library | License | Notes |
|---|---|---|---|
| CLI framework | `spf13/cobra` | Apache-2.0 | Subcommand tree matches §9 (`muro profile create`, `muro sandbox update`, ...) directly; free shell-completion generation. |
| Flag/config binding | `spf13/pflag` (via cobra) + `spf13/viper` *(optional, see note)* | BSD-3 / Apache-2.0 | Viper only if `daemon.yaml`/profile-file env-override binding gets unwieldy; otherwise plain `gopkg.in/yaml.v3` unmarshaling is enough — start without viper. |
| YAML parsing | `gopkg.in/yaml.v3` | MIT/Apache-2.0 dual | `daemon.yaml` only (§6) — small, flat, hand-edited settings file where comments and low punctuation-fuss matter. |
| JSON config & state | Go stdlib `encoding/json` | BSD-3 (Go stdlib) | Profiles (`profiles/<name>.json`, §6) and the state store (§5) both use this — no third-party dependency for either. |
| MQTT client (used by `murod`) | `eclipse/paho.mqtt.golang` | EPL-2.0 / EDL-1.0 | Mature, widely used, supports the pub/sub relay design in SPEC.md §8. |
| MQTT broker (`muro-broker`) | `mochi-mqtt/server` | MPL-2.0 | Pure-Go embeddable broker; `muro-broker` is a thin `main.go` around it (§4). |
| Namespace/syscalls | `golang.org/x/sys/unix` | BSD-3 | Needed regardless of the bwrap-wrapper decision (§6.1 of SPEC.md) for network-namespace setup, `/proc` bind-mount handling, etc. |
| Logging | stdlib `log/slog` | BSD-3 (Go stdlib) | Structured logging, no dependency; feeds both `muro logs` and on-disk daemon logs (§6). |
| Table rendering (`muro status`, `muro sandbox list`) | `olekukonko/tablewriter` | MIT | Renders the table-by-default output; JSON path bypasses this entirely. |
| Testing | stdlib `testing` + `stretchr/testify` (assertions only) | MIT | No mocking framework needed at this scope; hand-written fakes behind the `Isolator` interface (SPEC.md §6.1) are sufficient. |
| Unix domain socket control API | stdlib `net` + `net/rpc`-style JSON framing, hand-rolled | BSD-3 (Go stdlib) | See §7 — no gRPC in v1, deliberately, to avoid a codegen step and a protobuf toolchain dependency for a local-only IPC surface. |

No SQLite driver, no cgo dependency anywhere in the project (§5 explains why
SQLite was dropped from SPEC.md §7's original state-store sketch).

## 4. Binaries

Four separate binaries, one release, one repo — a fourth, `muro-shim`, was
added during implementation (not in the original design pass) once it
became clear `murod` holding a sandbox's pty directly meant every sandbox
died the instant `murod` exited, even on a clean restart:

- **`muro`** — the CLI client. Talks only to `murod`'s control socket. No
  root/CAP_* requirements; can run as the invoking user.
- **`murod`** — the daemon. Owns sandbox lifecycle, the isolator, the URL
  proxy, and the MQTT client connection to `muro-broker` (or any other MQTT
  broker). Requires whatever privilege the chosen isolator needs (§6.1 —
  unprivileged user namespaces via `bwrap`, no root required on a correctly
  configured host; see §9 host requirements).
- **`muro-broker`** — thin wrapper around `mochi-mqtt/server`, run as its own
  standalone process. Never embedded in `murod`. The *same* code path
  connects `murod` to a `muro-broker` on `localhost` (dev) or a self-hosted
  `muro-broker` on a remote host (production) — no branch in `murod` for
  "local vs. remote broker."
- **`muro-shim`** — a persistent per-sandbox pty holder, spawned by
  `BwrapIsolator.Launch` instead of `murod` exec'ing `bwrap` directly (the
  `containerd-shim`/`dtach` pattern). It allocates the pty, execs `bwrap` as
  its own child, and stays alive independent of `murod`'s process
  lifetime — relaying the pty over a per-sandbox Unix socket that `murod`
  (or a *restarted* `murod`, reconstructing a `Handle` from `state.json`)
  dials for `muro sandbox attach`. Its own lifetime is tied to the
  sandboxed process's, not `murod`'s: it exits shortly after its child
  does. Never invoked directly by an operator — an implementation detail of
  `BwrapIsolator`, the same way an operator never invokes `bwrap` directly
  either.

```
cmd/
  muro/       -> muro CLI binary
  murod/      -> murod daemon binary
  muro-broker/-> muro-broker binary (mochi-mqtt wrapper)
  muro-shim/  -> muro-shim binary (persistent per-sandbox pty holder)
internal/
  sandbox/    -> Sandbox Manager, Isolator interface + bwrap implementation
                 (bwrap.go spawns muro-shim; shim.go is the wire protocol
                 both BwrapIsolator and cmd/muro-shim speak)
  proxy/      -> URL-allowlist HTTP/CONNECT proxy
  control/    -> Unix-socket control API (server side, used by murod;
                 client side, used by muro)
  state/      -> JSON state store (§5)
  pubsub/     -> MQTT client wrapper, topic naming (SPEC.md §8)
  config/     -> profile + daemon.yaml loading/validation
```

## 5. State Store (revises SPEC.md §7)

SPEC.md §7 originally specified SQLite for `~/.local/state/muro/state.db`.
This design spec **replaces that with a JSON file**, for a specific
structural reason: `murod` is the sole reader and writer of this file — the
`muro` CLI never opens it directly, it only ever talks to `murod` over the
control socket (§7 below). There is exactly one writer process and no
concurrent-access case to protect against, so SQLite's transactional
guarantees buy nothing here, and dropping it removes a whole dependency axis
(cgo vs. pure-Go driver) from the project.

- **Location**: `~/.local/state/muro/state.json`.
- **Ownership model**: `murod` holds the authoritative sandbox registry in
  memory. `state.json` is a **persistence/crash-recovery snapshot**, not a
  live query interface — `muro status` is served from `murod`'s in-memory
  state via the control API, never by reading the file.
- **Write pattern**: on every state mutation (sandbox start/stop,
  reload-pending flag, allowlist update, new denied-URL event), `murod`
  serializes the full in-memory registry and writes it via
  **write-to-temp-file + `rename()`** (atomic on the same filesystem) —
  never a partial in-place write. This bounds corruption risk to "lose the
  last unwritten mutation on a hard crash between mutation and the next
  snapshot," which is acceptable given `murod` also re-derives live sandbox
  state (is the `bwrap` PID still alive?) on startup rather than trusting
  the snapshot blindly.
- **Denied-URL event log**: capped ring buffer (last *N*, `N` configurable
  in `daemon.yaml`, default 200) held in memory and included in the
  snapshot — no separate append-only log file in v1.
- **Startup reconciliation**: on `murod` start, load `state.json`, then for
  every sandbox marked `running`, check whether its recorded PID is alive
  and actually a `bwrap`/sandbox process; mark anything else `stopped
  (unclean)` rather than trusting stale state.

## 6. Filesystem Layout (confirms & extends SPEC.md §7)

Linux-only, XDG-conventioned:

```
~/.config/muro/
  daemon.yaml                  # control socket path, broker address,
                                #   topic root, log level, event-log cap
  profiles/<name>.json         # reusable sandbox profiles (mounts,
                                #   URL allowlist, env, default agent
                                #   command) — JSON, not YAML: profiles
                                #   are the one config object expected to
                                #   grow nested/list-heavy (mount entries,
                                #   allow/deny URL rules, env maps) and are
                                #   as likely to be generated/edited by
                                #   tooling as by hand

~/.local/state/muro/
  state.json                   # daemon-owned live sandbox registry (§5)
  logs/
    murod.log                  # daemon log (slog, JSON lines)
    sandbox/<namespace>__<name>.log   # per-sandbox stdout/stderr capture,
                                       # tailed by `muro logs <id> --follow`
  control.sock                 # Unix domain socket, control API (§7)

/usr/local/bin/  (or distro package path)
  muro
  murod
  muro-broker

/etc/systemd/system/  (or /usr/lib/systemd/system/ per distro packaging)
  murod.service
  muro-broker.service          # optional unit; not required if operator
                                # points daemon.yaml at a remote broker
```

- `control.sock` permissions: `0600`, owned by the invoking user — this is a
  single-operator-machine tool (SPEC.md §3 non-goals), so no group/ACL
  sharing model is designed for v1.
- Log rotation: not built into `murod` in v1 — documented as "point
  `logs/` at your distro's `logrotate` if you care," rather than
  reimplementing rotation. Revisit if this proves painful in practice.

## 7. Control API (murod ⇄ muro)

- **Transport**: Unix domain socket at `~/.local/state/muro/control.sock`.
- **Framing**: newline-delimited JSON request/response objects (not gRPC/
  protobuf) — deliberate choice to avoid a codegen toolchain for a
  local-only, single-client-at-a-time IPC surface. Revisit only if a future
  TUI/web client (SPEC.md §5, "just another client of the same control
  API") needs streaming semantics beyond what a long-lived connection +
  newline-delimited JSON events already gives it (e.g. `muro logs --follow`
  and `muro status --watch` both work fine as a held-open connection
  receiving a stream of JSON lines).
- Every `muro` subcommand maps to exactly one control API request type;
  the CLI itself contains no business logic beyond argument parsing,
  request construction, and response rendering (table or `--json`, §9).

## 8. Host Requirements (Linux only)

Explicit, since "Linux only" needs a concrete floor, not just a label:

- Kernel: unprivileged user namespaces must be enabled
  (`kernel.unprivileged_userns_clone=1` on Debian-family kernels that
  disable it by default; enabled by default on most other distros). `murod`
  checks this at startup and fails with a clear error + fix instructions
  rather than a cryptic `bwrap` failure.
- `bwrap` (bubblewrap) must be installed and on `PATH` — this is a hard
  runtime dependency given the §6.1 decision to wrap the `bwrap` binary
  rather than reimplement namespace management natively. `murod` checks for
  it at startup.
- **`slirp4netns` must be installed and on `PATH` — added during
  implementation, not originally listed here.** §6.2's proxy design
  requires each sandbox to reach `murod`'s allowlist proxy via a loopback
  address "only reachable from inside that sandbox's network namespace" —
  but a `bwrap --unshare-net` sandbox's loopback is fully private, with no
  route back to the host's, so without something bridging the two the
  proxy is literally unreachable and the entire URL-allowlist feature is
  inert. `slirp4netns` is the standard unprivileged fix for exactly this
  problem (the same tool rootless Podman/Docker use to give an unprivileged
  network namespace a route to a host service, without `CAP_NET_ADMIN` or
  root) — `murod` runs one `slirp4netns` instance per sandbox network
  namespace, giving that sandbox a route to `murod`'s proxy listener and
  nowhere else. `murod` checks for it at startup alongside `bwrap`.
- systemd assumed for service management (unit files shipped for `murod`
  and `muro-broker`); running either as a plain foreground process is still
  fully supported and documented, but packaging (unit files, `systemctl
  enable`) targets systemd distros specifically.
- No distro-specific packaging (`.deb`/`.rpm`) designed in v1 — static
  binaries + systemd unit files + an install script is the v1 distribution
  shape. Formal packages are future work.

## 9. CLI Surface (finalizes SPEC.md §9)

All commands are thin control-API clients (§7). Every listing/status command
supports `--json` in addition to the default table output.

`muro profile create` writes `~/.config/muro/profiles/<name>.json` (§6) —
flags populate an initial file; `muro profile edit` opens that JSON file in
`$EDITOR` (or accepts `--mount`/`--allow-url`/etc. flags again to patch it
without an editor) rather than the daemon holding profile state itself —
profiles are config, not runtime state, so `muro profile show`/`list` read
straight off disk with no daemon round-trip required.

**Sandbox addressing: human name, not the internal id.** Every
`muro sandbox ...`/`muro logs`/`muro sandbox attach` command below takes
`<agent-name>`, reusing the exact `namespace/name` addressing SPEC.md §8.4
already defines for pub/sub, rather than the daemon-generated internal id
(`sb_8f2a1c`-style) that only shows up in `state.json`/log filenames (§5,
§6) as a unique storage key. Resolution rule: a bare `<agent-name>` resolves
within the `default` namespace unless `--namespace <ns>` is passed, or the
full `<ns>/<agent-name>` form is given directly to disambiguate — same
default-namespace behavior as `muro run` (below). `murod` rejects an
ambiguous bare name the same way it already rejects a duplicate name at
launch: it can't be ambiguous, because names are only unique *within* a
namespace by construction.

```
muro daemon start|stop|status

muro profile create <name> [--mount ...] [--allow-url ...] [--agent ...]
muro profile edit <name>
muro profile list                          [--json]
muro profile show <name>                   [--json]

muro run --profile <name> --name <agent-name> [--namespace <ns>] \
          [--agent claude|gemini|custom ...]
                                            # namespace defaults to "default"
                                            # name must be unique within namespace

muro sandbox list [--namespace <ns>]       [--json]   # alias: muro ps
muro sandbox show <agent-name>             [--json]
muro sandbox update <agent-name> [--allow-url ...] [--deny-url ...] [--mount ...]
muro sandbox update --profile <name> [--namespace <ns>] \
                     [--allow-url ...] [--deny-url ...] [--mount ...]
                                            # fans the same delta out to every
                                            # running sandbox launched from
                                            # <name>, atomically (§11)
muro sandbox update --all [--allow-url ...] [--deny-url ...] [--mount ...]
                                            # every running sandbox, regardless
                                            # of profile — emergency-lockdown
                                            # escape hatch, atomic (§11)
muro sandbox reload <agent-name>           # apply pending config live where possible
muro sandbox restart <agent-name>          # apply everything, incl. non-hot-reloadable mounts
muro sandbox stop <agent-name>
muro sandbox attach <agent-name>           # take over the primary agent
                                            # process's stdio (§12) — for
                                            # agents that block on interactive
                                            # input, e.g. Claude Code CLI
                                            # permission prompts; single
                                            # exclusive attacher, see §12

muro logs <agent-name> [--follow]

muro status                                [--json]
                                            # table: id, agent, state, uptime,
                                            # mount count, allowlist rule count,
                                            # most recent denied-URL (if any)

muro broker status                         # connectivity to configured MQTT broker
```

Exit codes: `0` success, `1` general error, `2` usage/argument error, `3`
control-socket unreachable (murod not running).

## 10. Profile Schema Extension: Tool Restriction

SPEC.md §6.1's "deny by default" filesystem model already covers arbitrary
directories via `mounts:`, but nothing in SPEC.md restricts which
*executables* a sandbox can run — a mount that exposes a directory
implicitly exposes every binary in it. A profile therefore gets a second,
distinct field:

```json
"tools": [
  { "host": "/usr/bin/git", "as": "git" },
  { "host": "/usr/bin/node", "as": "node" },
  { "host": "~/.muro/toolchains/claude-default/bin", "as": "*" }
]
```

- Each entry is bind-mounted **read-only** into a fixed sandbox-internal
  location (e.g. `/usr/local/bin/<as>`), and the sandbox's `PATH` is set to
  *only* that location — no host `/usr/bin`, `/bin`, etc. is ever mounted
  wholesale. An empty (or omitted) `tools:` list means nothing is executable
  beyond `bwrap`'s own minimal scaffolding.
- `"as": "*"` mounts a whole host directory as the sandbox's tool root — this
  is the "use these tools from this installation directory" override: a
  profile can point at a curated/pinned/vendored toolchain directory the
  operator controls instead of touching host binaries at all, giving
  reproducible tool versions per profile independent of what's installed on
  the host.
- Mechanically this is not a new primitive — it's a mount with `PATH`
  implications — so it stays consistent with the existing mount model rather
  than introducing a second unrelated concept. `muro profile create` gains
  `--tool <host-path>[:<as>]` alongside `--mount`/`--allow-url`.
- **`tools:`/`mounts:` path collisions are rejected, not silently resolved.**
  Because a `tools:` entry's sandbox-internal target is exactly a mount with
  `PATH` implications, it can collide with an explicit `mounts:` entry that
  happens to target the same sandbox-internal path (e.g. a profile mounts
  something at `/usr/local/bin/git` directly *and* declares a `tools:` entry
  `as: git`). Silently letting one win would be a security-relevant footgun
  in a schema whose entire point is restricting what's executable, so
  instead: profile validation — run both client-side in `muro profile
  create/edit` and again by `murod` before every `muro run`/`reload`/
  `restart` (since profile JSON files can be hand-edited directly, §9) —
  rejects any `tools:`/`mounts:` pair whose resolved sandbox-internal paths
  overlap, with an error naming both conflicting entries. No mount/tool ever
  gets applied partially; the sandbox simply fails to launch/reload until
  the profile is fixed.

## 11. Bulk Sandbox Updates

A profile is a *template*: `muro run` copies its config into a new sandbox
at launch time (§9), so editing the profile file afterward does not
retroactively change already-running sandboxes — same model as a Compose
file vs. running containers. To intentionally fan a change out to every live
instance of a profile (e.g. two running `claude-default` sandboxes both need
a new mount or a new allowed URL), `muro sandbox update` accepts a selector
in place of a single `<id>` (§9):

- `--profile <name> [--namespace <ns>]` — every running sandbox launched
  from that profile (optionally scoped to one namespace).
- `--all` — every running sandbox, regardless of profile; an explicit
  escape hatch, not the default, so a typo can't silently reconfigure
  everything.

Both reuse the exact per-sandbox hot-reload path §6.3 (SPEC.md) already
defines — URL-allowlist changes are always hot; mount changes are hot only
where the isolator allows it, otherwise the sandbox is marked
`reload-pending`.

**Bulk updates are atomic, not best-effort: all targeted sandboxes succeed,
or none are changed.** `murod` validates the requested delta (including the
§10 tools/mounts collision check) against *every* sandbox the selector
matches before applying it to *any* of them. If any one sandbox in the
batch would fail validation — e.g. a `--mount` path that collides with a
`tools:` entry on one profile variant but not another, or a sandbox in the
batch having stopped between the request being built and applied — the
whole batch is rejected up front and the CLI reports exactly which sandbox
blocked it, with zero side effects on the rest. This matters specifically
because the selector can span multiple sandboxes with subtly different
existing config (§11 is not "identical clones," just "same profile at
launch time" — they may have already drifted via individual `muro sandbox
update <agent-name>` calls) — a partial apply would leave the batch in a
worse, inconsistent state than either applying cleanly or not applying at
all.

## 12. Interactive Access

Two distinct needs surfaced, and only one is in scope for v1:

- **In scope for v1 — `muro sandbox attach <agent-name>`.** Some agents (the
  Claude Code CLI in particular) block on interactive input — permission
  prompts, confirmations — not just file I/O, so their sandboxed process
  needs a real terminal, not just captured stdout/stderr. This requires
  `murod` to launch the agent process attached to a **pty it holds open**
  for the lifetime of the sandbox, rather than the plain
  output-capture-to-`logs/` model implied elsewhere in this document.
  `muro sandbox attach` reconnects the invoking terminal to that pty — raw
  mode, resize forwarding, full read-write — like `docker attach` or
  `tmux attach`. `muro logs --follow` remains a separate, read-only tail of
  the same captured output for when you just want to watch, not type.
- **Exactly one attacher at a time.** `murod` tracks attach state per
  sandbox (in-memory, not persisted in `state.json` — an attach session
  doesn't survive a daemon restart anyway once the pty's other end is gone).
  A second `muro sandbox attach <agent-name>` while one is already active is
  **rejected outright** — `sandbox already attached (since <time>)`, not
  queued and not silently multiplexed — since letting two terminals both
  drive raw input into the same interactive session is exactly the kind of
  footgun ("who actually typed that") this should avoid rather than solve
  cleverly. Detaching **without killing the agent** uses a fixed escape
  sequence, `Ctrl-P Ctrl-Q` (matching `docker attach`'s convention, so it's
  not a new thing to memorize) — after detach, the sandbox keeps running
  and a later `attach` can reconnect. `muro sandbox restart <agent-name>`
  (§9) always force-detaches first: a restart re-execs the agent process
  behind a brand-new pty, so any existing attach session is stale the
  moment restart runs, and `murod` closes it explicitly rather than leaving
  a client staring at a dead terminal.
- **Cut permanently — ad hoc shell exec.** A `docker exec -it`-style command
  that joins a running sandbox's namespaces to spawn a *second*, unrelated
  process (e.g. for debugging) is **not part of the roadmap**, not just
  deferred: `muro sandbox attach` already covers the real need (live access
  to the agent's own interactive session), and a second, separate
  namespace-join entry point (`nsenter -t <pid> -a`) adds an ongoing
  maintenance surface for a need that attach already satisfies.

## 13. Resource Limits, Crash Lifecycle, Broker ACLs, Name Collisions

**Resource limits (SPEC.md §10 item 2) — deferred to v2, unchanged.** No
cgroup CPU/memory caps in v1. Revisit once running many sandboxes in
parallel in practice actually shows host contention — not designed
speculatively ahead of that.

**Agent crash lifecycle (SPEC.md §10 item 3) — configurable per profile.**
Profile schema gains a `restart_policy` field alongside `mounts`/`tools`/
`allow_urls`:

```json
"restart_policy": "never"
```

- `"never"` (**default**) — on unexpected exit, `murod` marks the sandbox
  `crashed` and stops there; no auto-restart. This is the safe default
  because a silently-retried crash can mask a fundamentally broken profile
  (bad API key, a mount that no longer exists) behind a series of restarts
  that all fail the same way.
- `"on-failure"` — restart with exponential backoff, capped at a bounded
  number of attempts (`daemon.yaml`-configurable, default 5) before falling
  back to `crashed` behavior — same reasoning as `systemd`'s
  `Restart=on-failure` plus `StartLimitBurst`.
- `"always"` — restart unconditionally (backoff still applies to avoid a
  tight crash loop pinning a CPU), including after a clean exit.
- All three states publish through the existing `status` topic (SPEC.md
  §8) — its lifecycle event set (`started, stopped, reload-pending,
  restarted`) gains `crashed`, `restarting`, and `restart-exhausted`, so
  `on-failure`/`always` retry activity is visible over pub/sub, not just
  inferred from `muro status`.

**MQTT broker ACLs (SPEC.md §10 item 4) — resolved: daemon-side
enforcement only, no `muro-broker` ACL config in v1.** This was already the
shape of the design: sandboxes never connect to the broker directly (§8),
`murod` is the sole MQTT client and the only place that decides which
topics a given sandbox may use. A broker-level ACL would be defense-in-depth
for a gap that doesn't exist yet — it only becomes relevant once multiple
*mutually untrusted* `murod` instances might share one broker, which isn't
the v1 case (SPEC.md §3: single-operator machine(s)). `muro-broker` ships
with open access to whatever can reach it on the network; securing that
network path (firewalling `muro-broker`'s port, VPN/Tailscale between
machines, etc.) is an operator/deployment concern, not something `muro`
itself enforces in v1.

**Cross-daemon name collisions (SPEC.md §10 item 7) — resolved: reject on
broker-visible collision, implemented with a retained claim + MQTT Last
Will and Testament.** On starting a sandbox, `murod` publishes a **retained**
message to `muro/<root>/_claims/<namespace>/<name>` containing its own
daemon-instance id (generated once at `murod` startup, persisted in
`daemon.yaml`'s runtime state) and a timestamp; the message is cleared when
the sandbox stops cleanly. Before `muro run` launches a sandbox, `murod`
checks that claim topic — if a *live* claim from a **different** daemon
instance is already retained there, the launch is rejected outright with
`namespace/name already claimed by daemon <id> since <time>`, rather than
starting a second sandbox that would collide once both daemons reach the
same broker. Stale claims (a daemon that crashed without cleanly stopping
its sandboxes) are handled with MQTT's native **Last Will and Testament** —
**correction, found during implementation of `internal/pubsub`: MQTT allows
exactly one will message per connection, so "an LWT that clears all of a
daemon's claim topics" is mechanically impossible for any daemon running
more than one sandbox.** The actual mechanism: each `murod` also maintains a
single **per-daemon retained presence topic**,
`muro/<root>/_daemons/<daemon-id>/alive`, published `"online"` on connect
with its LWT set to publish `"offline"` there on unclean disconnect. A claim
is treated as stale not by the claim topic clearing itself, but by
correlating it against its claiming daemon's presence topic: before
rejecting a launch over a live-looking claim, `murod` also checks whether
that claim's daemon-id is currently `"online"` — if its presence topic says
`"offline"` (or has never been seen), the claim is stale and the launch
proceeds (publishing a fresh claim), rather than being blocked by a claim
its own daemon can no longer vouch for. This still reuses broker-native
retained-message + LWT semantics rather than inventing a custom reservation
protocol, just with one presence topic per daemon instead of one LWT
attempting to cover every claim topic that daemon owns.

## 14. Resolved vs. Still-Open (SPEC.md §10 cross-reference)

Resolved by this document (SPEC.md §10 items, in original numbering):
1. **Isolator choice** — wrap `bwrap` for v1 (still behind the `Isolator`
   interface per SPEC.md §6.1, so native namespace management remains
   swappable later without touching sandbox-management/proxy/pub-sub code).
2. **Resource limits** — deferred to v2, deliberately (§13): no cgroups in
   v1, revisit once parallel-sandbox contention is observed in practice.
3. **Agent crash lifecycle** — configurable per profile via
   `restart_policy: never|on-failure|always`, default `never` (§13).
4. **MQTT broker ACLs** — daemon-side enforcement only; no `muro-broker`
   ACL config in v1, since sandboxes never reach the broker directly and
   `murod` is already the sole enforcement point (§13).
5. **TLS-terminating proxy mode** — decided against permanently; see item
   14 below.
6. **Packaging/distribution** — four static Go binaries (a fourth,
   `muro-shim`, added during implementation — §4) + systemd units +
   install script, same repo/release (§4, §8).
7. **Cross-daemon name collisions** — reject on broker-visible collision,
   via a retained claim topic per `namespace/name` correlated against a
   single per-daemon presence topic (LWT-backed) to detect staleness from a
   crashed daemon — corrected from an earlier one-LWT-per-claim design that
   turned out to be mechanically impossible (§13).

Resolved in this conversation, not part of SPEC.md's original numbered list:
9. **CLI sandbox addressing** — `<agent-name>` (`namespace/name`, per
   SPEC.md §8.4), not the internal daemon-generated id (§9).
10. **Concurrent attach** — single exclusive attacher, rejected outright
    rather than queued/multiplexed; `Ctrl-P Ctrl-Q` detach; restart
    force-detaches (§12).
11. **`tools:`/`mounts:` path collisions** — rejected at profile validation
    time (client- and daemon-side), never silently resolved (§10).
12. **Bulk update batch semantics** — atomic, validated across the whole
    matched set before anything is applied to any of them (§11).
13. **Ad hoc shell exec** — cut permanently, not deferred; `muro sandbox
    attach` covers the real need (§12).
14. **TLS-terminating proxy mode** — decided against, permanently, not just
    deferred. Full HTTPS URL-*path* filtering is only achievable via a
    TLS-terminating MITM proxy (murod-generated CA installed inside every
    sandbox, live cert minting per host, decrypt/inspect/re-encrypt), which
    directly reverses SPEC.md §6.2's founding constraint ("must avoid
    managing a trusted root CA / self-signed certificates inside
    sandboxes") and breaks outright against any agent/SDK that does
    certificate pinning. Hostname-level (SNI) filtering — already speced in
    SPEC.md §6.2 and unchanged by this decision — is judged sufficient for
    the actual security goal: stopping a sandbox from reaching an
    unapproved/attacker-controlled host at all. Path-level granularity
    *within* an already-approved host is not being pursued.

15. **Multi-repo git worktree isolation for agent-driven changes** —
    per-repo `worktree: true` opt-in on the existing `git.repos` schema;
    muro creates and owns the worktree, `main`/the real checkout is never
    reachable from inside the sandbox, and merging back is a host-side-only,
    human-confirmed action (§15).
16. **`muro tui`** — a Bubble Tea terminal UI, delivering SPEC.md §5's
    always-anticipated "just another client of the same control API": a
    live-updating list of running sandboxes and launchable profiles,
    attach-and-switch in place of three separate one-shot commands (§16).

Nothing left open from SPEC.md §10's original list or from this
conversation.

## 16. `muro tui` — Terminal UI

**What it is.** A new `muro tui` subcommand (`internal/cli/tui.go` +
`internal/tui`), not a new binary — `murod` is untouched. Two tabs,
switched with `Tab`: *Running* (a permanent split — a list of sandboxes on
the left, a live console pane for whichever one is highlighted on the
right, both visible at once) and *Profiles* (`config.ListProfiles()`, no
daemon round-trip, same as `muro profile list`; `Enter` prompts for a name
and launches + auto-attaches). The list itself is polled via
`Client.Call(TypeStatus, ...)` every ~1.5s (`commands.go`'s `pollInterval`)
— `Client.Call` is strictly one-shot, no `--watch` request type exists
despite §7 musing about one, and adding server-push would mean touching
`murod`, which this feature deliberately avoids.

**Revision note:** an earlier version of this section described a simpler
"list, then attach full-screen" design (`Enter` handing the whole terminal
to the sandboxed agent via `tea.Exec`, `Ctrl-P Ctrl-Q` handing it back to
the list). That shipped first and worked, but direct feedback was specific:
the list should stay visible *while* watching/interacting with an agent,
not be replaced by it — see whoever's running, click, get its console right
there, switch easily. That's a materially different mechanism (below), not
a layout tweak, so this section was rewritten rather than amended in place.

**Mechanism: an embedded terminal emulator, not a byte passthrough.** A
pane that sits permanently next to a list can't hand the whole terminal to
the sandboxed agent the way `muro sandbox attach` does — Bubble Tea has to
keep rendering the list *at the same time* as the agent's own output. That
means actually parsing the agent's raw terminal output (cursor moves,
colors, its own redraws) into a screen buffer muro owns, then drawing that
into the pane — `internal/tui/pane.go`, backed by `github.com/hinshun/
vt10x` (confirmed real and usable: pkg.go.dev, packaged for Debian/Fedora/
Ubuntu). `vt10x.Terminal` embeds `io.Writer`, so the same raw attach stream
`Client.Attach` already returns is fed straight in (`io.Copy(term, r)`);
`renderPane` reads the resulting cell grid (`Cell(x, y) Glyph{Char, Mode,
FG, BG}`) and redraws it with `lipgloss` styling on a fixed ~80ms tick
(`paneTickCmd`), independent of and much faster than the status-list poll.
One real wrinkle, confirmed against the actual vt10x source: the attribute
bits in `Glyph.Mode` (bold/underline/reverse/...) are unexported constants
— `pane.go` replicates the same bit *values* locally, pinned by comment to
the exact vt10x version in `go.mod`, since there's no public name to import
them under. A future vt10x release renumbering them would silently
misrender attributes, not break the build — accepted, not fixed, since
there's no upstream export to depend on instead.

**Only one live attach connection at a time, still.** Moving the list
selection closes whatever was previously followed and opens a fresh attach
for the newly-highlighted sandbox — `internal/tui/pane.go`'s `session` /
`switchToCmd`. This is not "one pane visible of many live" — it's still
DESIGN.md §12's exclusive-attacher-per-sandbox model, just re-opened
automatically on every highlight instead of requiring an explicit
attach/detach cycle. Rapid selection changes are handled: `sessionOpenedMsg`
carries the target it was opened for, and the model tracks the most
recently *requested* target (`pendingTarget`) separately from the currently
*applied* one (`sessTarget`) — a slow dial that resolves after a faster,
later one already won is recognized as stale and its session is closed
immediately rather than clobbering the newer one.

**Keyboard focus, not a mode dialog.** The list owns keyboard focus by
default (arrows navigate; typed characters go nowhere). `Enter` moves focus
into the console — every keystroke, including arrows, is then forwarded to
the attached agent as raw bytes. `Ctrl-P Ctrl-Q` is not special-cased
client-side at all: it's forwarded like any other keystroke, and
`internal/control/stream.go`'s existing `detachScanner` recognizes it
server-side and ends the stream — exactly what already happens for `muro
sandbox attach`, completely unchanged. The pane's redraw tick is what
notices the resulting death (`session.Live()`, a non-blocking channel
check — no `*tea.Program` reference needed inside `pane.go` to push a
message) and returns focus to the list, freezing the pane on its last
frame rather than clearing it; nothing auto-reattaches on its own, since
that would silently reconnect right after an operator's own explicit
detach.

**Reconstructing raw bytes Bubble Tea already discarded.** Forwarding
keystrokes to the agent needs the *original* bytes a key was parsed from,
but Bubble Tea's input loop only hands `Update` the parsed `tea.KeyMsg`,
not the raw bytes — `internal/tui/keys.go`'s `keyBytes` rebuilds them.
Confirmed against Bubble Tea's actual source: every C0 control key's
`KeyType` numeric value *is* its raw control byte (`keyNUL=0` ... `keyESC=
27` ... `keyDEL=127`), so those need no lookup table at all; the "other
keys" block (arrows, home/end, etc.) uses negative `KeyType` values with no
such relationship, mapped by hand to their standard xterm/ANSI escape
sequences for the keys realistic to press while typing into a coding
agent's own prompt. F-keys and exotic ctrl+shift+arrow combos fall through
to nil (silently dropped) — a deliberate scoping choice, not an oversight.

**Two real bugs found and fixed via live pty-driven testing** (not
hypothetical — reproduced against the actual compiled binary, since a
full-screen program can't be meaningfully verified by reading code alone):

1. **Attaching to a non-running sandbox silently did nothing.** A stopped/
   crashed sandbox's attach stream just EOFs immediately — treated as a
   clean end, not an error, so the operator saw no explanation at all.
   Fixed with an explicit state check (`isAttachable`, mirrors
   `internal/sandbox.isActive`'s running/reload-pending/restarting set)
   before ever attempting to follow a highlighted sandbox, showing a clear
   inline error instead.

2. **In the earlier `tea.Exec`-based design, one attach/detach cycle
   permanently broke all keyboard input, including quit.** `ptyio.Pump`
   (`internal/ptyio/ptyio.go`, still used by the standalone `muro sandbox
   attach` command) returns as soon as EITHER direction of the byte-pump
   ends, not both — when the remote side ends first (the normal detach
   case), the *other* goroutine, still blocked reading `os.Stdin`, was
   never unblocked. Harmless for the standalone attach command (the whole
   process exits immediately after `Pump` returns, reaping the leak with
   it); fatal for a TUI that keeps running afterward and hands `os.Stdin`
   back to Bubble Tea — the leaked goroutine kept winning the race for
   every subsequent keystroke and discarding it. Fixed in `Pump` itself:
   `in` is wrapped in a `muesli/cancelreader.CancelReader` (already an
   indirect dependency via Bubble Tea, now direct), explicitly canceled
   once either side finishes, so no reader is ever left dangling against a
   shared fd after `Pump` returns. This fix predates and is independent of
   the later move away from `tea.Exec` for the Running tab — it stays
   correct and load-bearing for `muro sandbox attach`, which still uses
   `Pump` directly.

**Explicitly deferred, not attempted here: pty resize forwarding.**
Confirmed nothing in muro forwards terminal resize anywhere, for any attach
path — `internal/sandbox/bwrap.go`'s `OpenPTY` doc comment: *"it does not
set an initial terminal size... the attach path is expected to send a
resize once a real terminal is attached"*, never implemented. The console
pane renders whatever size the sandbox's pty already defaults to (matching
`muro sandbox attach`'s existing, already-working behavior) — cropped if
the pane is smaller, blank past the edge if larger — rather than the agent
laying out cleanly at the pane's actual width. Real resize support would
mean reaching all the way through `murod` into `muro-shim` (which actually
owns the pty, not `murod` itself): a new dedicated Unix socket from
`muro-shim` mirroring the existing `InjectSocketPath` pattern exactly
(`internal/sandbox/shim.go` + `cmd/muro-shim/main.go`), carrying
`TIOCSWINSZ` requests, plus a new control API request type for the CLI
side to report its own terminal resizes. Real, well-scoped, separate work —
deliberately not bundled into this pass so the interaction model could be
validated first.

**Shared code extracted to avoid an import cycle.** `internal/cli/tui.go`
constructs and runs the Bubble Tea program, so `internal/cli` must import
`internal/tui` — meaning `internal/tui` cannot import anything from
`internal/cli` back (raw-mode handling, the byte pump, and control-socket-
path resolution all used to live there, unexported). Both moved to lower
packages both sides can depend on without cycling: `internal/ptyio`
(`SetRawMode`, `Pump`) and `control.ResolveSocketPath()` (`internal/
control/socket.go`) — `muro sandbox attach` and `internal/cli/daemon.go`
were updated to call these instead of duplicating them, not left on two
diverging implementations.

**Three more real bugs, found through actual use of the shipped split
pane** (not the earlier pty-driven test harness — direct user feedback
after living with it):

1. **The Running list visibly reshuffled on every ~1.5s poll.**
   `internal/state.Store.List` built its result by ranging directly over
   its internal `map[string]*Sandbox` — Go's map iteration order is
   deliberately randomized, so two calls back-to-back could (and did)
   return the same sandboxes in a different order. Harmless for a one-shot
   `muro status` table; a real, visible bug for anything polling it
   continuously. Fixed at the source — `List` now sorts by namespace, then
   name, before returning — so every caller (`muro status`, `muro ps`,
   `muro tui`) gets a stable order, not just the TUI working around it
   itself.

2. **No reliable way back to the list once inside the console.** Ctrl-P
   Ctrl-Q (server-side detach, unchanged) is a two-key control-character
   chord — outside muro's control, a real terminal or multiplexer sitting
   between the operator and this process can intercept or remap either
   half of it before the process ever sees the bytes, and in practice this
   left the operator stuck in console focus with no way back. Fixed by
   giving `Esc` a client-side-only meaning: it always returns keyboard
   focus to the list immediately, no server round-trip, no ambiguity —
   *without* closing the session (`internal/tui/model.go`'s
   `handleKey`), unlike Ctrl-P Ctrl-Q which still ends the attach stream
   for real. Both are documented in the footer hint now; Esc is the
   reliable one.

   **Superseded by further real-use feedback**: Esc turned out to be a bad
   choice of key precisely because it's *not* rare — Claude Code (and other
   CLI agents) use Esc themselves, for cancel/clear, so making it a
   client-side-only interception meant every Esc press was silently eaten
   by the TUI and never reached the attached agent at all. Replaced with
   `F2`: Bubble Tea's own key encoding already never forwards F-keys to the
   agent (`keys.go`'s `keyBytes`: "unrecognized ... dropped, not guessed
   at"), so repurposing one as muro's own "back to the list" command
   changes nothing about what the agent used to receive — the single-key
   reliability Esc was chosen for, without Esc's collision with the agent's
   own keybindings. Esc itself now forwards through like any ordinary
   keystroke again.

3. **No scrollback — output that scrolled past was just gone.** vt10x
   itself tracks no history beyond its current screen (confirmed against
   the actual source: `scrollUp`/`scrollDown` just shift lines within the
   fixed-size grid, discarding what scrolls off — there's no separate
   history buffer to read back). Added a capped (512KB) raw-byte capture
   per session (`session.history`, `pane.go`), fed alongside — not instead
   of — the live `vt10x.Terminal`. Scrolling replays an earlier *prefix*
   of that buffer into a throwaway `vt10x.Terminal` and renders whatever
   screen that produces (`renderScrollback`/`scrollCutoff`) — the byte
   offset to replay up to is found by counting `\n` bytes backward from
   the end, since vt10x tracks no line index either. This is an
   approximation, not a perfect reconstruction, and deliberately so: a
   full-screen redrawing app doesn't have a meaningful scrollback concept
   to reconstruct in the first place, matching how real terminals also
   suspend scrollback during alt-screen mode. `PageUp`/`PageDown` scroll
   by one pane-height each while console-focused (intercepted, not
   forwarded to the agent — a real F-key-style tradeoff, same spirit as
   `keys.go`'s existing scoping); typing snaps back to live automatically,
   since interacting with the agent means you want to see it react.

   **Extended with mouse wheel support**: PgUp/PgDn alone meant scrolling
   only worked from the keyboard — the instinctive first thing anyone tries
   on a long console is the mouse wheel, and until this fix it silently did
   nothing (`muro tui`'s `tea.NewProgram` never enabled mouse mode at all).
   Fixed by adding `tea.WithMouseCellMotion()` (`internal/cli/tui.go`) and a
   `tea.MouseMsg` case in `Update` (`handleMouse`, `internal/tui/model.go`):
   wheel over the console pane scrolls it via the same `scrollByLines`
   primitive PgUp/PgDn now share (`scrollBack` is just `scrollByLines(dir *
   pageHeight)`), a few lines per notch rather than a full page for finer
   granularity; wheel while a list has focus moves its selection exactly
   like arrow keys, including following the Running-tab selection into a
   live attach the same way arrow keys do. Verified live via the pty-driven
   harness, sending real SGR mouse-wheel escape sequences
   (`\x1b[<64;x;yM`/`\x1b[<65;x;yM`) against the compiled binary attached to
   a real sandbox: the scroll indicator appeared and cleared exactly as
   expected, and wheel-on-the-list moved the highlighted sandbox.

4. **No way to start a stopped sandbox from inside `muro tui`.** With every
   real sandbox stopped (the common case between work sessions), Enter on a
   Running-tab item just showed "not running — nothing to attach to" — a
   dead end, since the list only ever showed sandboxes that already exist,
   with no separate affordance to bring one back up. `muro sandbox restart`
   already works on a stopped sandbox, not just a running one needing its
   config refreshed (confirmed live) — so Enter on a non-attachable item now
   calls it (`restartCmd`, `internal/tui/commands.go`) and auto-attaches
   once it comes back (`restartedMsg`'s handler in `model.go`'s `Update`),
   the same one-key affordance Enter already had for launching a brand-new
   sandbox from the Profiles tab, instead of erroring. Footer hint updated
   to `enter: type into it (or start, if stopped)`. Verified live via the
   pty-driven harness against a real stopped sandbox (`default/memtest`):
   Enter flips its state to `running` and the pane header/footer both
   correctly name it as the newly-attached target.

## 15. Multi-Repo Git Worktree Isolation for Agent-Driven Changes

**Problem.** Two agents (or two sandboxes from the same profile) working
against the same project must never be able to edit the same files at the
same moment — that's not a "which agent wins" question, it's silent data
loss. A project is also frequently *more than one* repo (frontend, backend,
shared libs, each its own checkout) mounted at different paths in the same
sandbox, so the fix has to work per-repo, not per-sandbox.

**Why this isn't solved with file locking.** Live locking between
independent processes is fragile (a crashed agent leaves a stale lock; a
lock doesn't stop a reader from seeing a half-written file mid-edit) and
doesn't compose with §6.1's "deny by default" mount model anyway. `git
worktree` gives the same guarantee structurally instead: each agent gets its
own working tree on its own branch, sharing one `.git` — two agents
literally cannot touch the same working-tree files, because each has its own
copy. Any real overlap between two agents' work surfaces later as an
ordinary git merge conflict, which is what git is for, rather than as
corrupted files nobody notices until later.

**Schema — extends §10's `GitRepoPolicy` (`internal/config/gitpolicy.go`),
does not replace it.** `GitPolicy.Repos` already supports multiple repos per
profile (it did before this feature existed — see correction below), each
scoping the git tool-proxy's branch/remote restrictions. Worktree isolation
is a new, explicit **opt-in** on top of that existing list, not implied by
merely appearing in it:

```json
"git": {
  "repos": [
    {
      "host": "~/projects/frontend",
      "allowed_remotes": ["origin"],
      "worktree": true,
      "mount_path": "/workspace/frontend"
    },
    {
      "host": "~/projects/backend",
      "worktree": true,
      "mount_path": "/workspace/backend"
    }
  ]
}
```

- `worktree` (new field, default `false`): opts this repo entry into the
  behavior below. **Not implicit from being listed under `git.repos`** —
  correcting course from earlier discussion in this conversation, since
  `git.repos` already had an existing, narrower meaning (tool-proxy
  restriction on a repo the profile mounts directly and unmodified) that a
  read-only/inspection-only repo entry may still legitimately want without
  ever getting an isolated, mergeable branch.
- `mount_path` (new field, **required when `worktree: true`, rejected
  otherwise**): the sandbox-internal path the generated worktree is mounted
  at. This inverts the existing rule for non-worktree entries — today,
  `Host` must be covered by a `mounts:` entry the profile itself declares
  (`ValidateProfile`, unchanged for `worktree: false`). For `worktree: true`
  entries, muro generates that mount itself, from the worktree it creates,
  so declaring a matching `mounts:` entry by hand would be redundant and is
  rejected by validation the same way an overlapping `tools:`/`mounts:` pair
  already is (§10).
- `allowed_branches` **must be left empty (or omitted) when `worktree:
  true`, and is rejected by validation if set** — corrected during
  implementation. `allowed_branches` restricts which branch a `commit`/
  `push` may land on (`internal/gitproxy`'s `CheckCurrentBranch`), but a
  worktree's branch is muro-generated (`agent/<namespace>/<name>`) — a
  profile author can't know that name in advance to write a matching
  pattern, and a wrong or missing one would silently block every commit the
  agent makes. `murod` computes the effective single-branch allowlist
  itself at launch/restart time (see "the actual isolation boundary"
  below), from the frontend example above that's exactly
  `["agent/<namespace>/<name>"]`, nothing else. `allowed_remotes` stays
  fully author-controlled and may legitimately be empty or omitted, as the
  `backend` example above shows — merging back is a host-side `murod`
  operation, not a sandbox push, so "no remote access at all, pure local
  commits" is the common, normal case, not a pointless empty declaration.

**Worktree creation — host-side, before the sandbox ever starts.** On
`muro run` (and on `restart --from-profile`, §9), for every `worktree: true`
repo entry, `murod`:
1. Runs `git worktree add <path> -b agent/<namespace>/<name> <base-branch>`
   against the real repo at `Host` — the sandboxed agent process never runs
   this itself.
2. Mounts that worktree path — not `Host` — at `mount_path` inside the
   sandbox, read-write.
3. `<base-branch>` is whatever `HEAD` resolves to on `Host` at launch time
   (typically `main`), not separately configurable in v1.

Step 1 is **idempotent** — added during implementation, since this section
didn't originally spell it out, but it's the only reading consistent with
the feature's own purpose: if the worktree path already exists on disk (a
`restart --from-profile` reusing the same sandbox ID's own worktree), `git
worktree add` is skipped entirely and the existing worktree, with whatever
commits the agent already made in it, is reused untouched. Without this, a
restart would either fail outright (the path already exists) or — far
worse if naively "fixed" by wiping and recreating — silently destroy the
agent's in-progress work. `<base-branch>` itself is remembered across a
restart via a small muro-owned sidecar file next to the worktree directory
(git has no first-class "this branch's base was X" relationship to query
back later), not re-derived from `Host`'s HEAD a second time.

**The actual isolation boundary — corrected during implementation.** An
earlier draft of this section claimed the sandbox "has no reachable path to
the real checkout or `.git` at all," full stop. That's not quite true, and
worth being precise about: a git worktree's own `.git` is a pointer file
into the real repo's `.git/worktrees/<name>/` — git commands run *from* a
worktree structurally require that shared metadata to be reachable, so
"nothing about the real repo is ever reachable by any means" is not
achievable while keeping git worktrees functional at all, and isn't actually
what this feature relies on for its guarantee anyway. The real mechanism
(confirmed via a genuine, real-bwrap end-to-end test,
`internal/sandbox/worktree_integration_test.go`): a `worktree: true` repo's
`GitRepoPolicy` is resolved with `Host` rewritten to the worktree's own
path, and — exactly like every other git-proxy-mediated repo already works,
`worktree: true` or not — the sandboxed process itself never gets a real
`git` binary or any `.git` path mounted at all; it only has
`muro-toolstub` on `PATH`, which forwards argv+cwd over a Unix socket to
`murod`. The real `git` process that touches `.git/worktrees/...` runs
**on the host, inside murod, unsandboxed** — the same trusted-execution
model this project's git tool-proxy has always used (§ "Tool Restriction,"
`internal/gitproxy`'s package doc). What the sandbox genuinely cannot do,
by construction, is: see or modify the real repo's *working-tree files*
(only the worktree's own checked-out copy is ever mounted), run any git
subcommand outside the daemon's fixed allowlist, or commit/push anywhere
but its own generated branch (`CheckCurrentBranch`/`AllowedBranches`,
already-existing gitproxy policy, unchanged by this feature). That's the
actual, buildable guarantee — not filesystem invisibility of `.git`, which
was never really the mechanism doing the protecting even before this
feature existed.

**Where worktrees live.** `~/.local/state/muro/sandboxes/<id>/worktrees/
<last-path-component-of-mount_path>/` — under `StateDir`, muro-owned end to
end, the same shape as the existing per-sandbox private-dir storage
(`.../sandboxes/<id>/private/...`, §6). Deliberately **not** next to the
real repo: creating scratch space there would need write access to the
user's actual project directories for something that's explicitly disposable
("only for the development purposes of the AI," not a browsable artifact the
way `~/.config/muro/profiles/` is meant to be) and risks colliding with the
user's own hand-created worktrees. Keying the directory name off
`mount_path`'s last component (not `Host`'s) is safe because `mount_path`
collisions across a sandbox's mounts are already rejected by the same
validation used for §10's `tools:`/`mounts:` overlap check.

**Surfaced to the user, not just to the agent.** `muro sandbox show`
(§9) gains a `worktrees` field: one entry per `worktree: true` repo,
listing `mount_path`, branch name, and whether it has commits not yet
merged to `<base-branch>` — so a human never has to rely on the agent
volunteering this in conversation to know what branch a sandbox is on.

**Merging back — host-side only, human-confirmed, squash.**
`muro sandbox merge <agent-name> [--repo <mount_path>]` (`--repo` required
only when a sandbox has more than one `worktree: true` entry):
1. Requires at least one real commit on the worktree's branch — dirty,
   uncommitted state alone is never "ready."
2. Shows the diff against `<base-branch>` and a proposed commit message,
   opened in `$EDITOR` for the operator to edit or approve outright —
   reusing exactly the pattern `muro profile edit` (§9) already uses
   (factored into a shared `openInEditor` helper), rather than inventing a
   second editor-invocation convention. **The proposed message is the
   agent's own last commit message on the worktree branch** (`git log -1
   --format=%B`) — corrected during implementation: muro has no channel for
   an agent to hand back a structured "summary," it's an opaque pty-driven
   process, but the accompanying `AGENT.md` convention already asks the
   agent to leave a clear final commit as its last action before saying
   it's done, so that commit's own message is the real, buildable source —
   nothing new required. The diff is shown as `#`-prefixed comment lines
   below the draft message, exactly `git commit`'s own editor convention.
3. On confirmation: `git merge --squash` into `<base-branch>` plus one
   commit with the operator's final message — the worktree branch's own
   possibly-messy incremental history never lands on `<base-branch>`, only
   this one commit does. **Preconditions, added during implementation:**
   the real checkout at `Host` must already be on `<base-branch>` with a
   fully clean `git status` before anything is touched. This isn't
   optional — a git worktree can never share a checked-out branch with
   another worktree (a hard git constraint), so the squash-merge has to run
   in whichever single working tree currently has `<base-branch>` checked
   out, normally the user's own primary checkout — `muro sandbox merge`
   never switches branches or stashes on the operator's behalf to work
   around this; it refuses with a clear error naming what to fix instead.
4. Only *after* a clean merge: prune the worktree (`git worktree remove`)
   and delete the branch. A conflicting merge aborts outright — no
   auto-resolution attempted, `git reset --hard HEAD` restores the real
   checkout to exactly its pre-attempt state (safe, since step 3's
   precondition just verified there was nothing else to lose) — leaving the
   worktree and branch exactly as they were; the operator resolves it by
   hand on the host, or by re-attaching the sandbox to have the agent
   reconcile it, then retries.

**Delete guard — extends the existing pattern, doesn't invent a new one.**
`muro sandbox delete` already refuses an active sandbox and already cleans
up a stopped sandbox's private dirs (§6) on success. It gains one more
check: refuse if any `worktree: true` repo has commits on its branch not yet
merged into `<base-branch>` — the existing `--yes` flag (confirming deletion
of sandbox metadata/logs) is **not** sufficient to also discard unmerged
code, since those are different classes of risk. Discarding on purpose
requires a distinctly-named `--discard-worktree <mount_path>`, once per
repo, so it can never be an accidental side effect of routine sandbox
cleanup. A worktree with nothing unmerged (never committed to, or already
merged) is pruned unconditionally as part of a normal delete, the same way
`RemovePrivateDirs` already runs unconditionally — nothing worth protecting
means nothing to ask permission for.

**Merging a mount out from under a still-running sandbox.** `muro sandbox
merge` doesn't require the sandbox to be stopped first — the merge itself
is a host-side git operation, independent of the sandbox process. But if
the sandbox IS currently active, its already-launched mount table still
includes the just-pruned worktree path; rather than leaving that silently
stale, the sandbox is marked `reload-pending` (the same existing state
`Update` already uses for a mount change that can't be hot-applied, §6.3)
so it's visible in `muro status`/`sandbox show` that a restart is needed.

**Agent-side convention (not enforced, courtesy only — belongs in
`AGENT.md`/`CLAUDE.md`, not in muro itself; §10's `Instructions`/`Skills`
fields already mount exactly this kind of content into a sandbox):** tell
the agent to mention its branch name early in conversation (redundant with
`sandbox show`, but nicer live UX), and — when it believes a change is
complete — to write a clear final summary that becomes the proposed merge
commit message, then say plainly that it cannot merge itself and is waiting
on the operator. This is honesty about a boundary that already exists
mechanically (the agent has no reachable path to `<base-branch>` or a merge
command), not a rule it has to remember to obey.

**What's explicitly unchanged.** A `git.repos` entry with `worktree`
omitted or `false` keeps exactly today's behavior — `Host` must be covered
by a hand-declared `mounts:` entry, no worktree is created, no
`mount_path`/`sandbox merge`/delete-guard behavior applies. Nothing about
existing profiles' git policy blocks changes unless they opt in.
