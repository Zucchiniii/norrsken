#!/bin/sh
# End-to-end test of the norrsken daemon on real Linux networking.
#
# Builds a tiny world in Docker:
#   nsk-api   fake NordVPN API                      172.30.0.10
#   nsk-sim1  WireGuard "NordVPN server" #1          172.30.0.11
#   nsk-sim2  WireGuard "NordVPN server" #2          172.30.0.12
#   nsk-tv    the TV: runs the real armv7 binary     172.30.0.20
# Each sim also owns 198.18.0.1 (a fake internet site that says which sim
# served it) and 103.86.96.100/103.86.99.100 (NordVPN's DNS addresses, here
# answered by dnsmasq), so traffic only reaches them through the tunnel.
#
# usage: test/integration.sh [path/to/norrsken-armv7]   (needs Docker)
set -eu

HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ROOT="$HERE/.."
BIN="${1:-}"
PLATFORM="${PLATFORM:-linux/arm/v7}"
NET=nsk-test
FAILS=0

if [ -z "$BIN" ]; then
  BIN="$(mktemp -d)/norrsken"
  (cd "$ROOT/daemon" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o "$BIN" .)
fi

cleanup() {
  docker rm -f nsk-api nsk-sim1 nsk-sim2 nsk-tv >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap '[ -n "${KEEP:-}" ] || cleanup' EXIT
cleanup

pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; FAILS=$((FAILS + 1)); }
check() { d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
tv() { docker exec nsk-tv "$@"; }
ctl() { docker exec nsk-tv norrsken ctl-json "$1"; }
field() { python3 -c 'import json,sys; d=json.load(sys.stdin); v=d.get("data") or {}
for k in sys.argv[1].split("."): v=(v or {}).get(k) if isinstance(v,dict) else None
print("" if v is None else v)' "$1"; }
wait_state() {
  want="$1"; limit="${2:-60}"; i=0
  while [ "$i" -lt "$limit" ]; do
    s="$(ctl '{"cmd":"status"}' 2>/dev/null || true)"
    case "$s" in *"\"state\":\"$want\""*) return 0 ;; esac
    sleep 1; i=$((i + 1))
  done
  echo "  (timed out waiting for $want; last status: $s)"
  return 1
}

echo "== building the test world"
docker network create --subnet 172.30.0.0/24 "$NET" >/dev/null
SIMBIN="$(mktemp -d)/simserver"
(cd "$ROOT/daemon" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$SIMBIN" ./cmd/simserver)
CLIENT_PRIV="$("$SIMBIN" genkey 2>/dev/null || docker run --rm -v "$SIMBIN:/s:ro" alpine:3.20 /s genkey)"
for n in 1 2; do
  docker run -d --name nsk-sim$n --network "$NET" --ip 172.30.0.1$n --cap-add NET_ADMIN --device /dev/net/tun \
    -v "$SIMBIN:/usr/local/bin/simserver:ro" alpine:3.20 sleep infinity >/dev/null
  docker exec nsk-sim$n apk add --no-cache -q dnsmasq python3 >/dev/null
done
CLIENT_PUB="$(docker exec nsk-sim1 simserver pubkey "$CLIENT_PRIV")"
for n in 1 2; do
  docker exec nsk-sim$n sh -c "
    simserver genkey > /k
    (simserver run \$(cat /k) $CLIENT_PUB > /sim.log 2>&1 &)
    sleep 1
    for a in 198.18.0.1 103.86.96.100 103.86.99.100; do ip addr add \$a/32 dev lo; done
    mkdir -p /www && echo via-sim$n > /www/index.html
    cd /www && (python3 -m http.server 8080 --bind 198.18.0.1 >/dev/null 2>&1 &)
    dnsmasq --no-resolv --bind-interfaces --listen-address=103.86.96.100,103.86.99.100 \
      --address=/test.norrsken/10.99.0.$n
  "
done
SIM1_PUB="$(docker exec nsk-sim1 sh -c 'simserver pubkey $(cat /k)')"
SIM2_PUB="$(docker exec nsk-sim2 sh -c 'simserver pubkey $(cat /k)')"
TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
CFG="$(mktemp)"
cat > "$CFG" <<EOF
{"token": "$TOKEN", "client_private_key": "$CLIENT_PRIV", "servers": [
 {"name": "Sweden #1", "hostname": "sim1.test", "ip": "172.30.0.11", "public_key": "$SIM1_PUB", "country": "Sweden", "code": "SE", "city": "Stockholm"},
 {"name": "Sweden #2", "hostname": "sim2.test", "ip": "172.30.0.12", "public_key": "$SIM2_PUB", "country": "Sweden", "code": "SE", "city": "Stockholm"}]}
EOF
docker run -d --name nsk-api --network "$NET" --ip 172.30.0.10 \
  -v "$HERE/fakeapi.py:/fakeapi.py:ro" -v "$CFG:/cfg.json:ro" python:3.12-alpine python3 /fakeapi.py /cfg.json >/dev/null

docker run -d --name nsk-tv --platform "$PLATFORM" --network "$NET" --ip 172.30.0.20 --privileged \
  --sysctl net.ipv6.conf.all.disable_ipv6=0 --sysctl net.ipv6.conf.default.disable_ipv6=0 \
  -e NORRSKEN_API=http://172.30.0.10:8000 -e NORRSKEN_STATE=/var/lib/norrsken \
  -e NORRSKEN_RUN=/run/norrsken -e NORRSKEN_INITD=/etc/init.test \
  alpine:3.20 sleep infinity >/dev/null
tv apk add --no-cache -q iptables curl bind-tools iproute2 >/dev/null
docker cp "$BIN" nsk-tv:/usr/local/bin/norrsken
HAVE_NAT=0; tv iptables -t nat -S OUTPUT >/dev/null 2>&1 && HAVE_NAT=1
# Docker starts eth0 with IPv6 off; turn it on so the leak guard has work to do.
tv sh -c 'echo 0 > /proc/sys/net/ipv6/conf/eth0/disable_ipv6' 
tv chmod 755 /usr/local/bin/norrsken
echo "   tv arch: $(tv uname -m), binary: $(tv norrsken version)"
sleep 1

echo "== account"
r="$(ctl '{"cmd":"setToken","token":"0000000000000000000000000000000000000000"}')"
case "$r" in *rejected*) pass "wrong token is rejected" ;; *) fail "wrong token is rejected ($r)" ;; esac
r="$(ctl "{\"cmd\":\"setToken\",\"token\":\"$TOKEN\"}")"
case "$r" in *'"linked":true'*) pass "token accepted, NordLynx key stored" ;; *) fail "token accepted ($r)" ;; esac
check "config.json is private (0600)" sh -c "docker exec nsk-tv stat -c %a /var/lib/norrsken/config.json | grep -qx 600"
check "token never appears in the log" sh -c "! docker exec nsk-tv grep -q '$TOKEN' /var/lib/norrsken/norrsken.log"

echo "== connect with server #1 down (expect failover to #2)"
docker exec nsk-sim1 pkill -STOP simserver
ctl '{"cmd":"connect","target":{"kind":"quick","label":"Fastest server"}}' >/dev/null
check "connects within 60 s" wait_state connected 60
s="$(ctl '{"cmd":"status"}')"
[ "$(echo "$s" | field server.hostname)" = "sim2.test" ] && pass "failed over to sim2" || fail "failover (server=$(echo "$s" | field server.hostname))"
check "internet traffic goes through the tunnel" sh -c "docker exec nsk-tv curl -s -m 5 http://198.18.0.1:8080/ | grep -qx via-sim2"
check "LAN stays direct (API host reachable)" tv curl -s -m 5 -o /dev/null http://172.30.0.10:8000/v1/servers/countries
check "routes: 0/1 and 128/1 via nordlynx" sh -c "docker exec nsk-tv ip route | grep -q '^0.0.0.0/1 dev nordlynx' && docker exec nsk-tv ip route | grep -q '^128.0.0.0/1 dev nordlynx'"
check "endpoint pinned to the real gateway" sh -c "docker exec nsk-tv ip route | grep -q '^172.30.0.12 via 172.30.0.1 dev eth0'"
check "IPv6 disabled on eth0 while connected" sh -c "docker exec nsk-tv cat /proc/sys/net/ipv6/conf/eth0/disable_ipv6 | grep -qx 1"
if [ "$HAVE_NAT" = 1 ]; then
  [ "$(echo "$s" | field dns)" = "redirect" ] && pass "DNS mode is iptables redirect" || fail "DNS mode ($(echo "$s" | field dns))"
  check "DNS to any server is answered by NordVPN DNS (via 8.8.8.8)" sh -c "docker exec nsk-tv dig +short +time=3 +tries=1 test.norrsken @8.8.8.8 | grep -qx 10.99.0.2"
  check "DNS to the router is answered by NordVPN DNS too" sh -c "docker exec nsk-tv dig +short +time=3 +tries=1 test.norrsken @172.30.0.1 | grep -qx 10.99.0.2"
  check "DNS over TCP is redirected" sh -c "docker exec nsk-tv dig +tcp +short +time=3 +tries=1 test.norrsken @9.9.9.9 | grep -qx 10.99.0.2"
else
  echo "SKIP  iptables DNS redirect (this kernel has no netfilter NAT)"
  [ "$(echo "$s" | field dns)" = "resolv" ] && pass "falls back to resolv.conf override" || fail "DNS mode ($(echo "$s" | field dns))"
  check "system resolver asks NordVPN DNS through the tunnel" sh -c "docker exec nsk-tv nslookup -timeout=3 test.norrsken | grep -q 10.99.0.2"
fi
ip=""; i=0
while [ -z "$ip" ] && [ "$i" -lt 15 ]; do ip="$(ctl '{"cmd":"status"}' | field insight.ip)"; [ -n "$ip" ] || sleep 1; i=$((i + 1)); done
[ -n "$ip" ] && pass "public-IP check filled in" || fail "public-IP check"

echo "== self-repair"
tv ip route del 172.30.0.12
sleep 3
check "deleted endpoint route is restored" sh -c "docker exec nsk-tv ip route | grep -q '^172.30.0.12 via'"
check "tunnel still carries traffic" sh -c "docker exec nsk-tv curl -s -m 5 http://198.18.0.1:8080/ | grep -qx via-sim2"
tv ip route del 128.0.0.0/1
sleep 3
check "deleted tunnel route is restored" sh -c "docker exec nsk-tv ip route | grep -q '^128.0.0.0/1 dev nordlynx'"

echo "== switch servers without dropping the interface"
docker exec nsk-sim1 pkill -CONT simserver
IFIDX="$(tv cat /sys/class/net/nordlynx/ifindex)"
ctl '{"cmd":"connect","target":{"kind":"country","countryId":208,"label":"Sweden","code":"SE"}}' >/dev/null
check "reconnects" wait_state connected 40
s="$(ctl '{"cmd":"status"}')"
[ "$(echo "$s" | field server.hostname)" = "sim1.test" ] && pass "now on sim1" || fail "switch (server=$(echo "$s" | field server.hostname))"
check "same interface (no leak window)" sh -c "[ \"\$(docker exec nsk-tv cat /sys/class/net/nordlynx/ifindex)\" = '$IFIDX' ]"
check "traffic now exits via sim1" sh -c "docker exec nsk-tv curl -s -m 5 http://198.18.0.1:8080/ | grep -qx via-sim1"
check "old endpoint route removed" sh -c "! docker exec nsk-tv ip route | grep -q '^172.30.0.12 '"

echo "== Threat Protection Lite DNS switch"
ctl '{"cmd":"setSettings","settings":{"threatProtection":true}}' >/dev/null
check "DNS follows the setting (filtered resolvers are not in the sim -> no answer)" sh -c "! docker exec nsk-tv nslookup -timeout=2 test.norrsken 2>/dev/null | grep -q 10.99"
ctl '{"cmd":"setSettings","settings":{"threatProtection":false}}' >/dev/null
check "back to standard NordVPN DNS" sh -c "docker exec nsk-tv nslookup -timeout=3 test.norrsken | grep -q 10.99.0.1"

echo "== kill switch"
ctl '{"cmd":"setSettings","settings":{"killSwitch":true}}' >/dev/null
check "blackhole routes installed" sh -c "docker exec nsk-tv ip route | grep -q '^blackhole 0.0.0.0/1'"
tv sh -c 'kill -9 $(cat /run/norrsken/daemon.lock)'
sleep 1
check "after a crash, internet is blocked" sh -c "! docker exec nsk-tv curl -s -m 3 http://198.18.0.1:8080/"
check "after a crash, LAN still works" tv curl -s -m 5 -o /dev/null http://172.30.0.10:8000/v1/servers/countries
r="$(ctl '{"cmd":"status"}')"
case "$r" in *'"state":"idle"'*) pass "app restarts the daemon, which recovers" ;; *) fail "recovery ($r)" ;; esac
check "blackholes removed after recovery" sh -c "! docker exec nsk-tv ip route | grep -q blackhole"
check "IPv6 restored after recovery" sh -c "docker exec nsk-tv cat /proc/sys/net/ipv6/conf/eth0/disable_ipv6 | grep -qx 0"
check "DNS chain removed after recovery" sh -c "! docker exec nsk-tv iptables -t nat -S | grep -q NORRSKEN"
check "default internet path is back" sh -c "docker exec nsk-tv ip route get 198.18.0.1 | grep -q 'via 172.30.0.1'"
ctl '{"cmd":"setSettings","settings":{"killSwitch":false}}' >/dev/null

echo "== resolv.conf fallback (TVs without iptables nat)"
ctl '{"cmd":"shutdown"}' >/dev/null; sleep 1
ORIG_RESOLV="$(tv md5sum /etc/resolv.conf)"
docker exec -e NORRSKEN_DNS_MODE=resolv nsk-tv norrsken ctl-json '{"cmd":"connect","target":{"kind":"quick","label":"Fastest server"}}' >/dev/null
check "connects" wait_state connected 40
s="$(ctl '{"cmd":"status"}')"
[ "$(echo "$s" | field dns)" = "resolv" ] && pass "DNS mode is resolv.conf override" || fail "DNS mode ($(echo "$s" | field dns))"
check "system resolver now asks NordVPN DNS" sh -c "docker exec nsk-tv nslookup -timeout=3 test.norrsken | grep -q 10.99.0"
ctl '{"cmd":"disconnect"}' >/dev/null
check "resolv.conf restored on disconnect" sh -c "[ \"\$(docker exec nsk-tv md5sum /etc/resolv.conf)\" = '$ORIG_RESOLV' ]"
check "IPv6 restored on disconnect" sh -c "docker exec nsk-tv cat /proc/sys/net/ipv6/conf/eth0/disable_ipv6 | grep -qx 0"
check "nordlynx interface gone" sh -c "! docker exec nsk-tv ip link show nordlynx"

echo "== connect at TV start-up"
ctl '{"cmd":"setSettings","settings":{"autoConnect":true}}' >/dev/null
check "init.d hook written" tv test -x /etc/init.test/60-norrsken
ctl '{"cmd":"shutdown"}' >/dev/null; sleep 1
tv sh /etc/init.test/60-norrsken
check "boot hook brings the VPN up by itself" wait_state connected 60
ctl '{"cmd":"setSettings","settings":{"autoConnect":false}}' >/dev/null
check "hook removed when turned off" sh -c "! docker exec nsk-tv test -e /etc/init.test/60-norrsken"
ctl '{"cmd":"shutdown"}' >/dev/null; sleep 1
check "shutdown cleans up the interface" sh -c "! docker exec nsk-tv ip link show nordlynx"

echo "== app update while the old daemon keeps running"
BIN2="$(mktemp -d)/norrsken"
(cd "$ROOT/daemon" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w -X main.buildID=update-test" -o "$BIN2" .)
ctl '{"cmd":"connect","target":{"kind":"country","countryId":208,"label":"Sweden","code":"SE"}}' >/dev/null
check "connected on the old build" wait_state connected 40
OLD_PID="$(ctl '{"cmd":"ping"}' | field pid)"
# Install the way webOS does: a new file takes the old one's place.
docker cp "$BIN2" nsk-tv:/usr/local/bin/norrsken.new
tv sh -c 'chmod 755 /usr/local/bin/norrsken.new && mv /usr/local/bin/norrsken.new /usr/local/bin/norrsken'
p="$(ctl '{"cmd":"ping"}')"
[ "$(echo "$p" | field build)" = "update-test" ] && pass "first request after the update runs the new build" || fail "new build took over ($p)"
[ "$(echo "$p" | field pid)" != "$OLD_PID" ] && pass "old daemon was replaced" || fail "old daemon still running (pid $OLD_PID)"
check "reconnects to the same place after the update" wait_state connected 40
# (exited processes linger as zombies here because the container's PID 1 is
# "sleep"; they have no command line, so pgrep -f skips them)
check "only one daemon is running" sh -c "[ \"\$(docker exec nsk-tv pgrep -f 'norrsken daemon' | wc -l | tr -d ' ')\" = 1 ]"
ctl '{"cmd":"shutdown"}' >/dev/null; sleep 1

echo
if [ "$FAILS" -eq 0 ]; then echo "ALL PASSED"; else echo "$FAILS FAILED"; echo "--- daemon log"; tv tail -40 /var/lib/norrsken/norrsken.log || true; fi
[ "$FAILS" -eq 0 ]
