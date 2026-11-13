#!/bin/sh
# Installs dist/*.ipk on a rooted LG TV over SSH (Homebrew Channel's sshd).
# usage: ./install.sh <tv-address> [path/to.ipk]
# Uses your normal SSH keys; set KEY=/path/to/key to pick one. Without an
# authorized key, ssh asks for the TV's root password.
set -eu
cd "$(dirname "$0")"
if [ $# -lt 1 ]; then echo "usage: $0 <tv-address> [path/to.ipk]" >&2; exit 2; fi
TV="$1"
IPK="${2:-$(ls dist/*.ipk 2>/dev/null | head -1)}"
[ -f "$IPK" ] || { echo "no IPK found; run ./build.sh or pass one" >&2; exit 1; }
SSH_OPTS="-o ConnectTimeout=8"
[ -n "${KEY:-}" ] && SSH_OPTS="$SSH_OPTS -o IdentitiesOnly=yes -i $KEY"
APP=com.zucchiniii.norrsken

echo "== copying $(basename "$IPK") to $TV"
# dropbear has no sftp server, so stream the file instead of using scp.
ssh $SSH_OPTS "root@$TV" 'cat > /tmp/norrsken.ipk' < "$IPK"

echo "== installing"
ssh $SSH_OPTS "root@$TV" sh -s <<EOF
set -e
# Stop a running daemon so the binary can be replaced cleanly.
B=/media/developer/apps/usr/palm/applications/$APP/bin/norrsken
[ -x "\$B" ] && "\$B" stop >/dev/null 2>&1 || true
luna-send -i -f luna://com.webos.appInstallService/dev/install \
  '{"id":"$APP","ipkUrl":"/tmp/norrsken.ipk","subscribe":true}' > /tmp/norrsken-install.log 2>&1 &
PID=\$!
i=0
while [ \$i -lt 90 ]; do
  if grep -q '"installed"' /tmp/norrsken-install.log 2>/dev/null; then break; fi
  if grep -qiE '"failed"|errorText' /tmp/norrsken-install.log 2>/dev/null; then break; fi
  sleep 1; i=\$((i + 1))
done
kill \$PID 2>/dev/null || true
if grep -q '"installed"' /tmp/norrsken-install.log; then
  echo "installed: \$("\$B" version 2>/dev/null || echo '?')"
else
  echo "install did not finish:"; cat /tmp/norrsken-install.log; exit 1
fi
rm -f /tmp/norrsken.ipk
EOF

if [ "${LAUNCH:-1}" = 1 ]; then
  ssh $SSH_OPTS "root@$TV" "luna-send -n 1 luna://com.webos.applicationManager/launch '{\"id\":\"$APP\"}' >/dev/null" && echo "== launched on the TV"
fi
