#!/bin/sh
# Run by the deb/rpm packages after install or upgrade. Creates the glean user
# and makes systemd see the units. The timer is left for the admin to enable:
# the first scan runs for hours, and nobody should get that by surprise.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers glean.conf || true
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
