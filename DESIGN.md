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
- Build: `CGO_ENABLED=0` for all three binaries — every chosen dependency
  (below) is pure Go, so static, fully self-contained binaries are the
  default build mode, not a stretch goal.
- Module layout: single Go module, single repo, three `cmd/` entrypoints
  (§4).

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

Three separate binaries, one release, one repo:

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

```
cmd/
  muro/       -> muro CLI binary
  murod/      -> murod daemon binary
  muro-broker/-> muro-broker binary (mochi-mqtt wrapper)
internal/
  sandbox/    -> Sandbox Manager, Isolator interface + bwrap implementation
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
6. **Packaging/distribution** — three static Go binaries + systemd units +
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

Nothing left open from SPEC.md §10's original list or from this
conversation.
