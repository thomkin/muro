#!/usr/bin/env bash
set -euo pipefail

# Entirely unprivileged: binaries go to $HOME/.local/bin (a standard
# per-user bin directory, already on PATH ahead of /usr/local/bin on most
# distros), and the systemd units are --user units under
# $HOME/.config/systemd/user. No sudo needed anywhere in this script — murod
# resolves its config/state dirs and the audio-passthrough $XDG_RUNTIME_DIR
# relative to whoever runs it (internal/config/paths.go), so it must run as
# the real desktop user, never as root, and there's no reason the install
# step itself needs root privilege either.

bin_dir="$HOME/.local/bin"
unit_dir="$HOME/.config/systemd/user"

mkdir -p "$bin_dir" "$unit_dir"

install -m 0755 bin/muro bin/murod bin/muro-broker bin/muro-shim bin/muro-toolstub bin/muro-quiet-chat "$bin_dir/"
install -m 0644 systemd/murod.service systemd/muro-broker.service "$unit_dir/"
systemctl --user daemon-reload

# enable (idempotent) + restart (starts it if not running yet, or restarts
# it onto the freshly installed binary if it was already running) — so a
# rebuild-and-reinstall always ends up actually running the new binary,
# not just leaving the old process running under the new unit file.
systemctl --user enable murod muro-broker
systemctl --user restart murod muro-broker

echo "Installed binaries to $bin_dir and user units to $unit_dir"
case ":$PATH:" in
	*":$bin_dir:"*) ;;
	*) echo "NOTE: $bin_dir is not on your \$PATH — add it in your shell config before running muro/murod directly." ;;
esac
echo "murod and muro-broker are enabled and running (systemctl --user status murod muro-broker to check)."
echo
if [ "$(loginctl show-user "$(whoami)" -p Linger --value 2>/dev/null)" != "yes" ]; then
	echo "To also start at boot without needing to log in first (needs sudo, one-time):"
	echo "  sudo loginctl enable-linger $(whoami)"
fi
