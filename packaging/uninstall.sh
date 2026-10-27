#!/bin/sh
# Removes mpts2spts installed from the tarball. --purge also deletes the
# configuration, database and service user.
set -e
[ "$(id -u)" = 0 ] || { echo "run as root: sudo $0" >&2; exit 1; }
systemctl disable --now mpts2spts.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/mpts2spts.service
systemctl daemon-reload || true
rm -rf /opt/mpts2spts
if [ "$1" = "--purge" ]; then
    rm -rf /etc/mpts2spts /var/lib/mpts2spts
    userdel mpts2spts >/dev/null 2>&1 || true
    echo "mpts2spts removed (configuration and data deleted)"
else
    echo "mpts2spts removed (kept /etc/mpts2spts and /var/lib/mpts2spts)"
fi
