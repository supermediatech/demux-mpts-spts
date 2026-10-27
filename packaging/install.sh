#!/bin/sh
# Installer for the mpts2spts tarball (any systemd Linux, x86-64).
# Usage: sudo ./install.sh
set -e
[ "$(id -u)" = 0 ] || { echo "run as root: sudo $0" >&2; exit 1; }
cd "$(dirname "$0")"

install -d /opt/mpts2spts
install -m 0755 mpts2spts /opt/mpts2spts/mpts2spts
install -m 0644 mpts2spts.env.example /opt/mpts2spts/mpts2spts.env.example
install -m 0644 setup-common.sh /opt/mpts2spts/setup-common.sh
install -m 0755 uninstall.sh /opt/mpts2spts/uninstall.sh
install -m 0644 mpts2spts.service /etc/systemd/system/mpts2spts.service

. /opt/mpts2spts/setup-common.sh
echo "  Uninstall: sudo /opt/mpts2spts/uninstall.sh [--purge]"
