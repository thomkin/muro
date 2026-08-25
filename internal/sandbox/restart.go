package sandbox

import "time"

// DefaultMaxRestartAttempts is the default cap on "on-failure" restart
// attempts before a sandbox is marked state.StateRestartExhausted
// (DESIGN.md §13) — matches daemon.yaml's documented restart_backoff_cap
// default. Manager.maxRestartAttempts is set from this by default;
// cmd/murod wires the real daemon.yaml value through when it's assembled.
const DefaultMaxRestartAttempts = 5

// backoffBaseDelay and backoffCapDelay bound the exponential backoff
// between restart attempts: 1s, 2s, 4s, 8s, 16s, then capped at 30s so a
// persistently crashing "always"-policy sandbox never grows its retry
// interval unboundedly.
const (
	backoffBaseDelay = time.Second
	backoffCapDelay  = 30 * time.Second
)

// backoffDelay returns the delay before restart attempt number `attempt`
// (1-indexed: the first retry is attempt 1).
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 { // 1s<<5 = 32s already exceeds the cap; avoid shift overflow for large counts
		return backoffCapDelay
	}
	d := backoffBaseDelay << uint(attempt-1)
	if d > backoffCapDelay {
		return backoffCapDelay
	}
	return d
}

// shouldRestart decides, per a sandbox's restart_policy (DESIGN.md §13),
// whether Manager should relaunch it after its process exited.
//
//   - "never" (including an empty/unrecognized policy): never restart.
//   - "on-failure": restart only after a non-clean exit, up to maxAttempts
//     total restart attempts; once exhausted the sandbox ends up
//     state.StateRestartExhausted rather than retrying forever.
//   - "always": restart unconditionally, including after a clean exit —
//     backoff still applies (via backoffDelay) so this can't tight-loop,
//     but there is deliberately no attempt cap, matching DESIGN.md §13's
//     "restart unconditionally".
func shouldRestart(policy string, restartCount, maxAttempts int, cleanExit bool) bool {
	switch policy {
	case "always":
		return true
	case "on-failure":
		if cleanExit {
			return false
		}
		return restartCount < maxAttempts
	default: // "never", or unrecognized
		return false
	}
}
