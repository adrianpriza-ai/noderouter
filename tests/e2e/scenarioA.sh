#!/bin/bash
# Cross-origin no-cors E2E (audit item 2). Run inside a loopback-only netns:
#   unshare -rn /tmp/e2e/scenarioA.sh <router-binary> <tag>
# Boots a mock upstream + router + attack-page collector, drives a real
# headless Firefox at the attack page, and reports whether the provider key
# reached the upstream.
set -u
BIN=$1; TAG=$2
D=$(cd "$(dirname "$0")" && pwd)
W=${E2E_WORKDIR:-/tmp/noderouter-e2e}/run-$TAG
rm -rf "$W"; mkdir -p "$W/tmp" "$W/home" "$W/ffprof" "$W/run"
ip link set lo up

C=18081; R=18082; U=18083
cat > "$W/config.json" <<EOF
{
  "server": {"host": "127.0.0.1", "port": $R, "maxBodyMb": 4, "requestTimeoutSecs": 10},
  "proxy": {"enabled": false, "test": {"enabled": false}},
  "provider": {"P": {"options": {"baseURL": "http://127.0.0.1:$U", "apiKey": "sk-providerkey"},
                     "models": {"m": {}}}}
}
EOF

sed "s|__TARGET__|http://127.0.0.1:$R/v1/chat/completions|" $D/attackA.html > "$W/attack.html"

python3 $D/upstream.py $U "$W/upstream.log" &
UP=$!
python3 $D/collector.py A $C $R "$W/results.log" "$W/attack.html" "$W/relay.log" &
COL=$!

HOME="$W/home" TMPDIR="$W/tmp" "$BIN" -config "$W/config.json" > "$W/router.log" 2>&1 &
ROUT=$!

for i in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:$R/healthz" && break
  sleep 0.2
done
if ! curl -s -o /dev/null "http://127.0.0.1:$R/healthz"; then
  echo "ROUTER DID NOT START"; cat "$W/router.log"; kill $UP $COL 2>/dev/null; exit 1
fi

export HOME="$W/home" TMPDIR="$W/tmp" XDG_RUNTIME_DIR="$W/run"
export MOZ_HEADLESS=1 MOZ_DISABLE_CONTENT_SANDBOX=1 MOZ_DISABLE_GMP_SANDBOX=1
chmod 700 "$W/run"
cp "$D/user.js" "$W/ffprof/user.js"
firefox --headless --no-remote --profile "$W/ffprof" "http://127.0.0.1:$C/attack.html" > "$W/firefox.log" 2>&1 &
FF=$!

for i in $(seq 1 60); do
  grep -q 'fetch-\(resolved\|rejected\)' "$W/results.log" 2>/dev/null && break
  sleep 0.5
done
sleep 1

kill $FF 2>/dev/null; sleep 1; kill -9 $FF 2>/dev/null
kill $UP $COL 2>/dev/null
kill $ROUT 2>/dev/null; sleep 0.5; kill -9 $ROUT 2>/dev/null

echo "===== [$TAG] page reports (browser side) ====="
cat "$W/results.log" 2>/dev/null || echo "(none - attack JS never ran)"
echo "===== [$TAG] upstream saw (provider side) ====="
cat "$W/upstream.log" 2>/dev/null || echo "(nothing - provider key not spent)"
echo "===== [$TAG] router log (tail) ====="
tail -5 "$W/router.log"
echo "===== [$TAG] firefox log (tail) ====="
tail -5 "$W/firefox.log"
