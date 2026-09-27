#!/bin/sh
# Install wakegate-node on the on-demand node. Run as root from the repo root
# after `make build-node` (or with a downloaded linux binary at bin/wakegate-node).
set -eu
install -m 0755 bin/wakegate-node /usr/local/bin/wakegate-node
install -m 0644 deploy/node/wakegate-node.service /etc/systemd/system/wakegate-node.service
install -m 0644 deploy/node/wakegate-node-shutdown.service /etc/systemd/system/wakegate-node-shutdown.service
systemctl daemon-reload
systemctl enable --now wakegate-node-shutdown.service wakegate-node.service
