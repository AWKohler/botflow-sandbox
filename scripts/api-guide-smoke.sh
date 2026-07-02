#!/usr/bin/env bash
#
# Runnable version of the docs/api-testing-guide.md §12 checklist. Exercises the
# raw HTTP API a tester would use (curl only, no SDK) end to end against a live
# deployment — public Funnel URL by default.
#
# Usage:
#   BASE=https://ai-club-pc-ms-7c56.taila01548.ts.net/api \
#   TOKEN=<admin token> \
#   [FORCE_PUBLIC=1] ./scripts/api-guide-smoke.sh
#
#   FORCE_PUBLIC=1 pins the connection to the public Funnel ingress IP so the
#   run proves internet reachability even from a machine on the tailnet.
#
# Requires: bash, curl, jq, python3, dig (only if FORCE_PUBLIC=1).
# Exit code 0 = every check passed.

set -uo pipefail

BASE=${BASE:?set BASE, e.g. https://<host>/api}
TOKEN=${TOKEN:?set TOKEN to the admin bearer token}
PROJECT=${PROJECT:-default}
TEAM=${TEAM:-default}
SBX=${SBX:-guide-smoke-$$}

# --- optional: force the public Funnel path ---------------------------------
RESOLVE=()
HOST=$(printf '%s' "$BASE" | sed -E 's#https?://([^/]+).*#\1#')
if [[ ${FORCE_PUBLIC:-0} == 1 ]]; then
  pubip=$(dig +short @1.1.1.1 "$HOST" | head -1)
  [[ -n "$pubip" ]] || { echo "FORCE_PUBLIC set but $HOST has no public DNS record" >&2; exit 1; }
  RESOLVE=(--resolve "$HOST:443:$pubip")
  echo "forcing public path: $HOST -> $pubip"
fi

auth=(-H "Authorization: Bearer $TOKEN")
pass=0 fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
# api METHOD PATH [json-body]  -> sets $HTTP (status) and $BODY (response)
api() {
  local method=$1 path=$2 body=${3:-}
  local args=("${RESOLVE[@]}" "${auth[@]}" -sS -X "$method" -w $'\n%{http_code}')
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  local resp; resp=$(curl "${args[@]}" "$BASE$path")
  HTTP=${resp##*$'\n'}; BODY=${resp%$'\n'*}
}
jqr() { printf '%s' "$BODY" | jq -r "$1" 2>/dev/null; }
# run a shell script inside the session, capture combined stdout, set $GUEST_OUT.
# Retries once on empty output to absorb the brief readiness gap on the first
# command after a fresh resume.
guest() {
  local script=$1 timeout=${2:-60000} attempt
  local body; body=$(jq -nc --arg s "$script" --argjson t "$timeout" \
    '{cmd:"bash",args:["-lc",$s],wait:true,logs:true,timeout:$t}')
  for attempt in 1 2; do
    local resp; resp=$(curl "${RESOLVE[@]}" "${auth[@]}" -sS --max-time 180 \
      -H 'Content-Type: application/json' -d "$body" \
      "$BASE/v2/sandboxes/sessions/$SID/cmd")
    GUEST_OUT=$(printf '%s' "$resp" | python3 -c '
import json,sys
out=[]
for line in sys.stdin:
    line=line.strip()
    if not line: continue
    try: obj=json.loads(line)
    except Exception: continue
    if "data" in obj: out.append(obj["data"])
sys.stdout.write("".join(out))')
    [[ -n "$GUEST_OUT" ]] && return
    sleep 2
  done
}

cleanup() { curl "${RESOLVE[@]}" "${auth[@]}" -sS -X DELETE \
  "$BASE/v2/sandboxes/$SBX?projectId=$PROJECT&teamId=$TEAM" -o /dev/null || true; }
trap cleanup EXIT

echo "== 1. health + auth gating =="
health=$(curl "${RESOLVE[@]}" -sS "${BASE%/api}/health")
[[ "$(printf '%s' "$health" | jq -r .status 2>/dev/null)" == ok ]] && ok "GET /health => ok" || bad "GET /health ($health)"
code=$(curl "${RESOLVE[@]}" -sS -o /dev/null -w '%{http_code}' "$BASE/v2/sandboxes?projectId=$PROJECT&teamId=$TEAM")
[[ "$code" == 401 ]] && ok "unauthenticated => 401" || bad "unauthenticated => $code (want 401)"

echo "== 2. create + runtimes =="
api POST /v2/sandboxes "{\"name\":\"$SBX\",\"runtime\":\"node24\",\"resources\":{\"vcpus\":1},\"timeout\":600000,\"ports\":[4173]}"
SID=$(jqr '.session.id')
[[ "$HTTP" == 201 && -n "$SID" && "$SID" != null ]] && ok "create => $SID" || { bad "create ($HTTP): $BODY"; echo "aborting"; exit 1; }
guest 'node --version; python3.13 --version; whoami; pwd'
grep -q 'v24' <<<"$GUEST_OUT" && grep -q '3.13' <<<"$GUEST_OUT" && grep -q vercel-sandbox <<<"$GUEST_OUT" \
  && ok "node24 + python3.13 present, running as vercel-sandbox in /vercel/sandbox" \
  || bad "runtime/env check: $GUEST_OUT"

echo "== 3. vite + react + tailwind build =="
guest 'set -e; cd /vercel/sandbox; rm -rf app; pnpm create vite@latest app --template react >/dev/null 2>&1;
cd app; pnpm install --frozen-lockfile=false >/dev/null 2>&1; pnpm add tailwindcss @tailwindcss/vite >/dev/null 2>&1;
pnpm exec vite build >/dev/null 2>&1 && test -f dist/index.html && echo VITE_OK' 300000
grep -q VITE_OK <<<"$GUEST_OUT" && ok "pnpm vite+react+tailwind build" || bad "vite build: ${GUEST_OUT: -300}"

echo "== 4. egress allowlist =="
guest 'printf allowed=; curl -s -o /dev/null -w "%{http_code}" --max-time 10 https://registry.npmjs.org/react; echo;
printf blocked=; curl -s --max-time 5 https://example.com >/dev/null 2>&1 && echo REACHED || echo blocked'
grep -Eq 'allowed=(200|30[0-9])' <<<"$GUEST_OUT" && grep -q 'blocked=blocked' <<<"$GUEST_OUT" \
  && ok "npm reachable, example.com blocked" || bad "egress: $GUEST_OUT"

echo "== 5. ecosystem packages (convex/stripe/claude-code) =="
guest 'set -e; cd /vercel/sandbox/app; pnpm add convex stripe @stripe/stripe-js @anthropic-ai/claude-code >/dev/null 2>&1;
pnpm exec convex --version >/dev/null 2>&1 && pnpm exec claude --version >/dev/null 2>&1 && echo ECO_OK' 240000
grep -q ECO_OK <<<"$GUEST_OUT" && ok "convex + stripe + claude-code install" || bad "ecosystem: ${GUEST_OUT: -300}"

echo "== 6. filesystem persistence across stop/resume =="
guest 'echo persist-marker > /vercel/sandbox/keep.txt; cat /vercel/sandbox/keep.txt'
grep -q persist-marker <<<"$GUEST_OUT" && ok "wrote marker file" || bad "write marker: $GUEST_OUT"
api POST "/v2/sandboxes/sessions/$SID/stop"
[[ "$HTTP" == 200 ]] && ok "stop => 200" || bad "stop => $HTTP"
api POST "/v2/sandboxes/sessions/$SID/cmd" '{"cmd":"bash","args":["-lc","true"]}'
[[ "$HTTP" == 410 ]] && ok "command on stopped session => 410" || bad "stopped session => $HTTP (want 410)"
api GET "/v2/sandboxes/$SBX?projectId=$PROJECT&teamId=$TEAM&resume=true"
NEWSID=$(jqr '.session.id')
[[ "$HTTP" == 200 && -n "$NEWSID" && "$NEWSID" != "$SID" ]] && ok "resume => new session $NEWSID" || bad "resume ($HTTP): new=$NEWSID old=$SID"
SID=$NEWSID
guest 'cat /vercel/sandbox/keep.txt'
grep -q persist-marker <<<"$GUEST_OUT" && ok "marker survived stop/resume" || bad "persistence: $GUEST_OUT"

echo "== 7. snapshot + create-from-snapshot =="
api POST "/v2/sandboxes/sessions/$SID/snapshot" '{}'
SNAP=$(jqr '.snapshot.id')
[[ "$HTTP" == 200 && -n "$SNAP" && "$SNAP" != null ]] && ok "snapshot => $SNAP" || bad "snapshot ($HTTP): $BODY"
if [[ -n "$SNAP" && "$SNAP" != null ]]; then
  FORK="$SBX-fork"
  api POST /v2/sandboxes "{\"name\":\"$FORK\",\"runtime\":\"node24\",\"source\":{\"type\":\"snapshot\",\"snapshotId\":\"$SNAP\"}}"
  FSID=$(jqr '.session.id')
  if [[ "$HTTP" == 201 && -n "$FSID" && "$FSID" != null ]]; then
    SID=$FSID guest 'cat /vercel/sandbox/keep.txt'   # guest() retries on the first-command race
    grep -q persist-marker <<<"$GUEST_OUT" && ok "fork from snapshot has the marker" || bad "fork content: $GUEST_OUT"
  else
    bad "create-from-snapshot ($HTTP): $BODY"
  fi
  curl "${RESOLVE[@]}" "${auth[@]}" -sS -X DELETE "$BASE/v2/sandboxes/$FORK?projectId=$PROJECT&teamId=$TEAM" -o /dev/null
fi

echo "== 8. isolation probes (all must fail from inside the guest) =="
# step 7's snapshot stopped the session; resume to get a running one to exec in.
api GET "/v2/sandboxes/$SBX?projectId=$PROJECT&teamId=$TEAM&resume=true"
SID=$(jqr '.session.id')
[[ "$HTTP" == 200 && -n "$SID" && "$SID" != null ]] || bad "resume before isolation ($HTTP)"
guest 'f=0
curl -s --max-time 4 http://169.254.169.254/ >/dev/null 2>&1 && { echo METADATA_REACHED; f=1; }
curl -s --max-time 4 https://1.1.1.1/ >/dev/null 2>&1 && { echo RAWIP_REACHED; f=1; }
curl -s --max-time 4 http://100.100.100.100/ >/dev/null 2>&1 && { echo TAILNET_REACHED; f=1; }
ls -l /dev/kvm >/dev/null 2>&1 && { echo KVM_PRESENT; f=1; }
[ $f -eq 0 ] && echo ISO_OK'
grep -q ISO_OK <<<"$GUEST_OUT" && ok "metadata/raw-IP/tailnet/kvm all denied" || bad "isolation: $GUEST_OUT"

echo "== 9. concurrency cap (best-effort) =="
made=(); over=0
for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
  api POST /v2/sandboxes "{\"name\":\"$SBX-c$i\",\"runtime\":\"node24\",\"resources\":{\"vcpus\":1},\"timeout\":120000}"
  if [[ "$HTTP" == 201 ]]; then made+=("$SBX-c$i"); elif [[ "$HTTP" == 429 ]]; then over=1; break;
  elif [[ "$HTTP" == 503 ]]; then echo "  (host at capacity before hitting the token cap; skipping)"; break; fi
done
[[ "$over" == 1 ]] && ok "per-token concurrency cap returns 429" || bad "expected a 429 within 12 creates (made ${#made[@]})"
for n in "${made[@]}"; do curl "${RESOLVE[@]}" "${auth[@]}" -sS -X DELETE "$BASE/v2/sandboxes/$n?projectId=$PROJECT&teamId=$TEAM" -o /dev/null; done

echo
echo "==== $pass passed, $fail failed ===="
[[ "$fail" == 0 ]]
