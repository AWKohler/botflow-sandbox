#!/usr/bin/env bash
set -euo pipefail

CREDENTIALS=${CREDENTIALS:-/home/ai-club-pc/sandbox-host-credentials}
BASE_URL=${BASE_URL:-http://127.0.0.1:8080}
# shellcheck disable=SC1090
source "$CREDENTIALS"

auth=(-H "Authorization: Bearer $SANDBOX_TOKEN")
record=$(curl -fsS "${auth[@]}" "$BASE_URL/api/v2/sandboxes/smoke-node24?projectId=$SANDBOX_PROJECT_ID")
session=$(jq -er '.session.id' <<<"$record")

guest_script=$(cat <<'GUEST'
set -o pipefail
printf 'uid='; id -u
node --version
pnpm --version
python3.13 --version
printf 'allowed_npm='
curl -fsS --max-time 10 -o /dev/null -w '%{http_code}\n' https://registry.npmjs.org/react
printf 'blocked_example='
if curl -fsS --max-time 5 https://example.com >/dev/null 2>&1; then echo FAIL; else echo blocked; fi
printf 'blocked_google_search='
if curl -fsS --max-time 5 'https://www.google.com/search?q=x' >/dev/null 2>&1; then echo FAIL; else echo blocked; fi
GUEST
)
body=$(jq -nc --arg script "$guest_script" '{cmd:"bash",args:["-lc",$script],wait:true,logs:true}')
curl -sS --max-time 45 "${auth[@]}" -H 'Content-Type: application/json' -d "$body" \
  "$BASE_URL/api/v2/sandboxes/sessions/$session/cmd"
