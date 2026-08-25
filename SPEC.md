# muro — Sandboxed Multi-Agent Runtime

## 1. Summary

muro is a Linux sandboxing framework for running multiple AI coding agents
(Claude Code CLI, Gemini CLI, custom agent CLIs, etc.) in parallel, each isolated
inside a Bubblewrap-style sandbox with a runtime-configurable filesystem view and
a URL-level network allowlist. A single background daemon owns all sandboxes,
persists their configuration, exposes live status, and relays messages between
agents over a pub/sub bus that is designed to work across machines.

The core problem this solves: give an AI agent enough filesystem and network
access to do its job, and nothing else — enforced by the OS and an in-process
proxy, not by the agent's own good behavior — while making it practical to run
many such agents at once and see what they're all doing.

## 2. Goals

- Run each agent inside a Linux namespace sandbox with an explicit, per-sandbox
  set of bind-mounted directories (nothing else on the host filesystem is
  visible).
- Enforce a per-sandbox network allowlist at the URL/host level, without
  routing traffic through an external proxy like Squid.
- Let both the mount list and the URL allowlist be changed at runtime, with
  changes hot-reloaded into a running sandbox wherever technically possible.
- Run many sandboxes in parallel on one machine and see their status from one
  place.
- Let sandboxes exchange messages over pub/sub, with a design that supports a
  self-hosted broker reachable over the network — so agents on different
  machines can eventually participate in the same pub/sub topics — even though
  early implementation and testing happens against a local broker.
- Store all configuration and state on the local machine in a well-defined
  location, independent of any single agent CLI.

## 3. Non-Goals (v1)

- Not a general-purpose container runtime or Docker/Podman replacement — no
  image registry, no OCI image format support.
- Not a multi-tenant or multi-user system — designed for one operator's
  machine(s).
- Not a hardened security boundary against a maliciously adversarial workload
  (e.g. not intended to safely run untrusted third-party code against attacker
  models) — the threat model is "keep a well-behaved but unpredictable AI
  agent inside its lane," not "contain a hostile exploit chain." Should still
  follow sound sandboxing practice, but this scoping affects prioritization.
- No GUI/web dashboard in v1 (CLI table output only; TUI/web is future work).
- No full TLS-terminating MITM inspection in v1 (see §6.2 for the tradeoff
  this implies).

## 4. Key Terms

- **Sandbox**: one isolated process tree running one agent, with its own
  mount namespace and network policy.
- **Agent**: the program run inside a sandbox (e.g. `claude`, `gemini`, a
  custom CLI).
- **Daemon (`murod`)**: the long-running background service that owns
  sandbox lifecycle, the proxy, and the pub/sub connection.
- **Profile**: a named, reusable sandbox configuration (mounts + URL
  allowlist + env) that can be applied when launching a sandbox.
- **Namespace**: a named grouping that scopes agent-name uniqueness. The
  same agent name can exist in different namespaces without colliding, and
  namespace also scopes topic addressing so unrelated agent groups (or
  environments) don't cross-talk by default.

## 5. Architecture Overview

```
        ┌──────────┐
        │ muro CLI │
        └────┬─────┘
             │  control API (local Unix socket)
             ▼
   ┌────────────────────────────────────────────────┐
   │                 murod (daemon)                 │
   │                                                │
   │  ┌─────────────┐    ┌───────────────────────┐  │
   │  │   Sandbox   │    │  URL-allowlist proxy  │  │
   │  │   Manager   │    │  (per-sandbox rules)  │  │
   │  └─────────────┘    └───────────────────────┘  │
   │  ┌─────────────┐    ┌───────────────────────┐  │
   │  │    bwrap    │    │     pub/sub client    │  │
   │  │ process(es) │    │         (MQTT)        │  │
   │  └─────────────┘    └───────────────────────┘  │
   │                                                │
   │       state store (SQLite/JSON on disk)        │
   └──┬─────────────────────────────────────────────┘
      │
      MQTT broker (local or remote)
      other murod instances /
      other machines
```

- `murod` is the only process that talks to the kernel (launches `bwrap`),
  runs the proxy, and holds the pub/sub connection.
- `muro` (the CLI) is a thin client: every command is a request to the
  daemon's control API over a local Unix domain socket. This is what makes
  hot-reconfiguration, live status, and pub/sub relaying work without hacks.
- A future TUI/web dashboard would be just another client of the same control
  API — no daemon changes needed.

## 6. Sandboxing

### 6.1 Isolation mechanism

- **Decision deferred to implementation spike, not locked here.** Two
  candidates, both acceptable, to be evaluated early in implementation:
  1. **Wrap the `bwrap` binary** — `murod` generates `bwrap` CLI arguments
     from a sandbox's mount/env config and execs it. Least code, reuses a
     mature, audited tool.
  2. **Native Go namespace management** — `murod` calls
     `unshare`/`clone`/`pivot_root` directly (e.g. via a Go syscalls library)
     without depending on the `bwrap` binary being installed.
  - Explicitly ruled out: Docker/Podman (too heavyweight, image-oriented,
    wrong abstraction for "a directory view + a URL allowlist") and
    firejail/nsjail (no strong reason to prefer over bwrap; adds another
    unfamiliar dependency).
  - Whichever is chosen, it must sit behind an internal `Isolator` interface
    in the Sandbox Manager so the other choice can be swapped in later
    without touching sandbox-management, proxy, or pub/sub code.
- Each sandbox gets its own mount namespace, PID namespace, and (for the
  network proxy to work) its own network namespace with loopback-only access
  plus a route to the daemon's proxy.
- Filesystem: **deny by default**. A sandbox sees only the host paths
  explicitly listed as mounts in its config (read-only or read-write, per
  mount entry), plus whatever minimal `/proc`, `/dev`, `/tmp` scaffolding the
  agent needs to run at all.

### 6.2 Network policy — URL allowlist proxy

Requirements driving this design: filtering must be **URL-based** (not just
IP-based), must be **built into `murod`** (no external Squid/mitmproxy
process to run and manage separately), and must **avoid managing a trusted
root CA / self-signed certificates** inside sandboxes.

These three requirements are partially in tension, and the spec resolves it
as follows:

- **Plain HTTP**: `murod`'s embedded proxy sees the full request
  (`Host` + path) in cleartext and can allow/deny on the complete URL.
- **HTTPS (the common case)**: without terminating TLS, the proxy cannot see
  the request path — only the **SNI hostname** from the TLS ClientHello (for
  `CONNECT`-style traffic) or the destination host. **v1 filters HTTPS by
  hostname (+ port), not full URL path**, and then passes the encrypted
  bytes through untouched — no CA, no cert generation, no decryption. This is
  the direct consequence of "no self-signed certs" + "don't care if it's
  encrypted, just pass it through."
- This tradeoff must be stated explicitly to the user in `muro` output when
  they add an HTTPS rule with a path component: the path is accepted in
  config for forward-compatibility but only the host is actually enforced
  until/unless a TLS-terminating mode is added later as an opt-in, separate
  feature (out of scope for v1; a sandbox that opts in would need a
  generated per-sandbox CA installed inside it).
- Implementation shape: sandboxes are given `HTTP_PROXY`/`HTTPS_PROXY`
  pointing at a loopback address only reachable from inside that sandbox's
  network namespace; `murod` runs one embedded Go proxy (e.g. built on
  `net/http` + raw `CONNECT` handling) that looks up the calling sandbox's
  ID (by which loopback/namespace the connection came from) and applies that
  sandbox's current allowlist.
- Default policy is **deny all**; an empty allowlist means the sandbox has no
  network access at all.
- Denied requests are logged (with sandbox id, timestamp, host/URL) and
  surfaced via `muro logs <sandbox>` and the pub/sub `sandbox.network.denied`
  event (see §8).

### 6.3 Runtime configuration & hot-reload

- Mount list and URL allowlist both live in the sandbox's config and can be
  changed via `muro sandbox update <id> --allow-url ... --mount ...` (or by
  editing the sandbox's config file and running `muro sandbox reload <id>`).
- **URL allowlist changes apply live**: the proxy holds an in-memory ruleset
  per sandbox and simply swaps it — no kernel-level operation needed, so this
  is always hot-reloadable with no restart.
- **Mount changes apply live where the isolation mechanism allows adding/
  removing bind mounts on a running mount namespace**; where that isn't
  possible for a given mount type, `murod` marks the sandbox
  `reload-pending` and the CLI reports exactly which requested changes were
  applied live vs. require `muro sandbox restart <id>`. This distinction
  must be visible in `muro status` output, not silently swallowed.

## 7. Configuration & State Storage

- Location: `~/.config/muro/` for user-edited configuration,
  `~/.local/state/muro/` for daemon-owned runtime state (XDG conventions).
- `~/.config/muro/profiles/<name>.yaml` — reusable named profiles (mounts,
  URL allowlist, env, default agent command).
- Every sandbox config (a profile, or a one-off `muro run`) carries a
  **`name`** (required, unique within its namespace) and a **`namespace`**
  (optional, default `default`). `murod` rejects launching a sandbox whose
  `name` is already active in the same namespace. This name is what other
  agents use to address it — see §8.4.
- `~/.config/muro/daemon.yaml` — daemon-level settings (control socket
  path, pub/sub broker address, MQTT topic root, log level).
- `~/.local/state/muro/state.db` — SQLite database owned by `murod`:
  live sandbox registry (id, profile used, pid, state, start time, current
  effective mounts/allowlist, last N denied-URL events). This is the source
  of truth for `muro status`; config files are just input, not live state.
- All state is local-machine-only in v1; no assumption of shared/networked
  config storage between machines (each machine runs its own `murod`).

## 8. Multi-Agent Coordination (Pub/Sub)

- **Protocol: MQTT.** Chosen because it has mature, simple Go client
  libraries, lightweight self-hosted broker options (e.g. Mosquitto, EMQX),
  and native support for a broker reachable over the network — satisfying
  "design for cross-machine from day one" without inventing a custom wire
  protocol.
- Each `murod` instance is one MQTT client connection; sandboxes do not
  connect to MQTT directly — they publish/subscribe via the daemon's control
  API (or a small in-sandbox helper the daemon injects), so the daemon can
  enforce which topics a given sandbox may use.
- Broker deployment is pluggable: `daemon.yaml` points at a broker address.
  For local development, that's a broker running on `localhost` (e.g. a
  Mosquitto instance started alongside `murod`, or embedded if a suitable
  Go MQTT broker library is used). For cross-machine use, it's a
  self-hosted broker on a reachable host — same protocol, same daemon code
  path, no v2 rework required.
- **Shared brokers are expected.** The same MQTT broker may well have other,
  unrelated applications publishing on it (home-automation tooling, other
  self-hosted services, another muro deployment entirely). Every topic muro
  publishes or subscribes to is therefore rooted under a single **topic
  root**, `muro/` by default, so it can't collide with anything else on the
  broker. The root is a `daemon.yaml` setting (`mqtt.topic_root`), not a
  hardcoded string — lets two independent muro installs share one broker
  without collision (e.g. `mqtt.topic_root: muro-home` vs `muro-work`), and
  survives a future project rename without a silent behavior change.
- Standard topic namespace (subject to refinement during implementation),
  rooted under the configured topic root and then keyed by
  **`<namespace>/<agent-name>`** rather than by host or sandbox id — see
  §8.4 for why (all shown below with the default root, `muro`):
  - `muro/<namespace>/<agent-name>/status` — lifecycle events (started,
    stopped, reload-pending, restarted).
  - `muro/<namespace>/<agent-name>/net-denied` — denied network requests.
  - `muro/<namespace>/<agent-name>/inbox` — direct messages addressed to
    this specific agent.
  - `muro/<namespace>/broadcast/<topic>` — free-form, namespace-scoped
    topics agents publish/subscribe to for their own coordination.
- v1 implementation and testing target is a local broker; remote-broker
  configuration is part of the same code path and should be exercised at
  least once in testing, but is not the primary day-to-day setup initially.

### 8.4 Agent naming & addressing

- Every agent is identified by its **`namespace/name`** pair, set in its
  sandbox config (see §7), not by host or sandbox id. This is what makes
  addressing work across machines by design: as long as two `murod`
  instances point at the same broker, one agent can publish to another
  agent's `inbox` topic by name alone, regardless of which machine it's
  actually running on.
- `murod` relays `inbox` messages to the sandbox via the daemon's control
  API — sandboxes still never connect to MQTT directly (§8, first bullet).
- Name uniqueness is enforced by `murod` **within its own namespace, on
  its own machine** in v1. Cross-machine uniqueness (two daemons
  independently starting the same `namespace/name` before either is
  connected to a shared broker) is not yet resolved — see open question 07.

## 9. CLI Surface (v1)

Thin client over the daemon's control API. Indicative command set:

```
muro daemon start|stop|status

muro profile create|edit|list|show <name>

muro run --profile <name> [--agent claude|gemini|custom ...] \
           --name <agent-name> [--namespace <ns>]   # name unique per namespace; default namespace is "default"
muro sandbox list [--namespace <ns>]   # alias: muro ps
muro sandbox show <id>
muro sandbox update <id> [--allow-url ...] [--mount ...] [--deny-url ...]
muro sandbox reload <id>               # apply pending config live where possible
muro sandbox restart <id>              # apply everything, including non-hot-reloadable mounts
muro sandbox stop <id>
muro logs <id> [--follow]

muro status                            # table: id, agent, state, uptime, mounts, urls, pending-reload?
```

Status output columns (v1, table only — no TUI): sandbox id, agent type,
state (running/stopped/reload-pending), uptime, mount count, allowlist rule
count, most recent denied-URL (if any).

## 10. Open Questions / Deferred Decisions

These are flagged rather than resolved here, and should be revisited once
implementation starts:

1. **Isolator choice** (§6.1): `bwrap` wrapper vs. native Go namespace code —
   spike both minimally before committing.
2. **Resource limits**: cgroups (CPU/memory caps per sandbox) are not yet
   scoped — likely needed once running many agents in parallel in practice.
3. **Agent lifecycle semantics**: what happens on agent crash — auto-restart,
   leave stopped, notify via pub/sub? Not yet decided.
4. **Auth/ACL on the MQTT broker**: even for local dev, should sandboxes be
   restricted to a topic prefix by broker-level ACL, or only by daemon-side
   enforcement? Matters more once cross-machine is actually turned on.
5. **TLS-terminating mode** (§6.2): full URL-path filtering for HTTPS is
   explicitly deferred; revisit if the SNI-only granularity proves
   insufficient in practice.
6. **Packaging/distribution**: single static Go binary assumed; installer /
   systemd unit for `murod` not yet designed.
7. **Cross-daemon name collisions** (§8.4): name uniqueness is only enforced
   per-daemon in v1. If two machines each start an agent with the same
   `namespace/name` before both are connected to a shared broker, addressing
   collides. Needs a resolution strategy (reject on broker-visible
   collision? require a reservation step against the broker?) before
   cross-machine use is relied on for real.
