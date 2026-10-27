# Shared install steps, sourced by the .deb postinst and install.sh.
# Creates the service user, data dir and config (with a random password on
# first install), then enables and (re)starts the service.
set -e

CONF_DIR=/etc/mpts2spts
CONF=$CONF_DIR/mpts2spts.env
DATA_DIR=/var/lib/mpts2spts

if ! getent passwd mpts2spts >/dev/null; then
    useradd --system --home-dir "$DATA_DIR" --no-create-home \
        --shell /usr/sbin/nologin --comment "mpts2spts service" mpts2spts
fi

install -d -m 0750 -o mpts2spts -g mpts2spts "$DATA_DIR"
install -d -m 0755 "$CONF_DIR"

NEW_PASSWORD=""
if [ ! -f "$CONF" ]; then
    NEW_PASSWORD=$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | head -c 16)
    sed "s/__PASSWORD__/$NEW_PASSWORD/" /opt/mpts2spts/mpts2spts.env.example > "$CONF"
fi
chown root:mpts2spts "$CONF"
chmod 0640 "$CONF"

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable mpts2spts.service >/dev/null 2>&1 || true
    systemctl restart mpts2spts.service || true
fi

PORT=$(sed -n 's/^LISTEN=.*:\([0-9]*\)$/\1/p' "$CONF" | tail -1)
echo
echo "  mpts2spts is installed and running."
echo "  Web UI:  http://$(hostname -I 2>/dev/null | awk '{print $1}'):${PORT:-8080}/"
if [ -n "$NEW_PASSWORD" ]; then
    echo "  Login:   admin / $NEW_PASSWORD"
    echo "  (stored in $CONF; change it there and restart the service)"
fi
echo
