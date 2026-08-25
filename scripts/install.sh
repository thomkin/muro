#!/usr/bin/env bash
set -euo pipefail

install -m 0755 bin/muro bin/murod bin/muro-broker /usr/local/bin/
install -m 0644 systemd/murod.service systemd/muro-broker.service /etc/systemd/system/
systemctl daemon-reload
echo "Installed. Enable with: systemctl enable --now murod muro-broker"
