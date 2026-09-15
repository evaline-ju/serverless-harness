#!/usr/bin/env bash
# deploy/knative/demo-multiuser.sh
# MU1 multi-user demo (docs/specs/2026-09-08-multi-user-control-plane-design.md §10).
#
# The claim: one deployment serves many users, each authenticating as themselves, holding their own
# credentials, and seeing only their own work -- with the isolation resting on properties the process
# cannot violate rather than on care.
#
# Proved with two real GitHub accounts, because the whole point is that the subject is ATTESTED by
# GitHub rather than asserted by the caller. The script cannot fabricate two subjects.
#
# WHAT THIS DEMO DOES NOT SHOW, said out loud rather than implied:
#   * The SANDBOX POOL IS SHARED in slice 1 -- two users' leaves can be placed on the same pod.
#     Isolation here holds at the API, the session store and the inference credential, NOT the
#     sandbox. The tenant-labelled partition is MU2 (spec §8.2).
#   * Both users' stored credentials may hold the same upstream key. What is demonstrated is that each
#     turn runs on the credential the CONTROL PLANE resolved for that subject -- and that a subject
#     with none cannot run at all, with the deployment's own key sitting in the environment.
#
# Runs with SH_REQUIRE_AUTH=true, so the property demonstrated is the real one and not the permissive
# default. The flag is restored on exit.
#
# Prereqs, all manual and one-time (spec §5.1.1):
#   * A warm harness cluster (setup-kind.sh) whose image contains MU1.
#   * A registered GitHub OAuth app with DEVICE FLOW ENABLED (it is off by default), exported as
#     SH_GITHUB_CLIENT_ID. No client secret is needed -- the device flow treats the app as a public
#     client, which is why this is safe to run from a script you can read.
#   * Two GitHub accounts, to be Alice and Bob.
#   * MULTIUSER_LIVE_SMOKE=1, matching the env-gated live-smoke convention.
#   * jq, curl, kubectl, openssl.
# Usage:
#   MULTIUSER_LIVE_SMOKE=1 SH_GITHUB_CLIENT_ID=Iv1.xxxx bash deploy/knative/demo-multiuser.sh
#   bash deploy/knative/demo-multiuser.sh --teardown
set -euo pipefail
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
cd "$(dirname "$0")"
source ./lib.sh # NS, BASE, HOST_HEADER, CURL_HDR, CURL_OPTS, ok/ko, PASS/FAIL, set_ksvc_env

CP_PORT="${SH_DEMO_CP_PORT:-18080}"
CP_BASE="http://localhost:${CP_PORT}"
TEARDOWN=0
for a in "$@"; do
  case "$a" in
    --teardown) TEARDOWN=1 ;;
    -h | --help)
      sed -n '2,40p' "$SELF"
      exit 0
      ;;
    *)
      echo "unknown flag: $a" >&2
      exit 2
      ;;
  esac
done

PFS=()
RESTORE_FLAG=0
cleanup() {
  for pid in ${PFS[@]+"${PFS[@]}"}; do kill "$pid" 2>/dev/null || true; done
  # Restore the permissive default even on an aborted run: leaving SH_REQUIRE_AUTH=true would make
  # every other smoke in this directory 401 on /turn.
  if [ "$RESTORE_FLAG" -eq 1 ]; then
    set_ksvc_env SH_REQUIRE_AUTH=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [ "$TEARDOWN" -eq 1 ]; then
  echo "== tearing down"
  set_ksvc_env SH_REQUIRE_AUTH=false SH_SESSION_TOKEN_PUBLIC_KEYS= >/dev/null 2>&1 || true
  kubectl delete -f control-plane.yaml --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete secret sh-session-token-key sh-credential-kek sh-exchange-token -n "$NS" \
    --ignore-not-found >/dev/null 2>&1 || true
  echo "  done"
  exit 0
fi

# --- gates -------------------------------------------------------------------------------------
# SKIP, not fail: the prerequisites are manual and outside any script, so an unset gate means "not
# configured here", exactly as the other env-gated live smokes treat it.
if [ "${MULTIUSER_LIVE_SMOKE:-0}" != "1" ]; then
  echo "SKIP: set MULTIUSER_LIVE_SMOKE=1 to run the multi-user demo (needs a cluster + 2 GitHub accounts)"
  exit 0
fi
if [ -z "${SH_GITHUB_CLIENT_ID:-}" ]; then
  echo "SKIP: SH_GITHUB_CLIENT_ID is unset."
  echo "      Register a GitHub OAuth app, ENABLE device flow (it is off by default), and export its"
  echo "      client id. No client secret is needed. See spec §5.1.1."
  exit 0
fi
for tool in jq curl kubectl openssl; do
  command -v "$tool" >/dev/null || {
    echo "SKIP: $tool is required"
    exit 0
  }
done

# --- stand up the control plane ----------------------------------------------------------------
echo "== deploying the control plane (opt-in; not part of the base kustomization)"
if ! kubectl get secret sh-session-token-key -n "$NS" >/dev/null 2>&1; then
  KEYFILE="$(mktemp)"
  openssl genpkey -algorithm ed25519 -out "$KEYFILE" 2>/dev/null
  kubectl create secret generic sh-session-token-key -n "$NS" \
    --from-file=SH_SESSION_TOKEN_PRIVATE_KEY="$KEYFILE" >/dev/null
  # The data plane receives only the PUBLIC half, as plain config: a compromised harness can verify
  # but not mint (spec §5.2).
  PUBDER="$(mktemp)"
  openssl pkey -in "$KEYFILE" -pubout -outform DER >"$PUBDER" 2>/dev/null
  KID="$(openssl dgst -sha256 -hex "$PUBDER" | awk '{print substr($2,1,16)}')"
  PUBKEYS="${KID}:$(base64 <"$PUBDER" | tr -d '\n')"
  shred -u "$KEYFILE" "$PUBDER" 2>/dev/null || rm -f "$KEYFILE" "$PUBDER"
  echo "$PUBKEYS" >/tmp/sh-demo-pubkeys
else
  PUBKEYS="$(cat /tmp/sh-demo-pubkeys 2>/dev/null || true)"
  [ -n "$PUBKEYS" ] || {
    echo "FAIL: sh-session-token-key exists but its public half is unknown; run --teardown first"
    exit 1
  }
fi
kubectl get secret sh-credential-kek -n "$NS" >/dev/null 2>&1 ||
  kubectl create secret generic sh-credential-kek -n "$NS" \
    --from-literal=SH_CREDENTIAL_KEK="$(openssl rand -base64 32)" >/dev/null
kubectl get secret sh-exchange-token -n "$NS" >/dev/null 2>&1 ||
  kubectl create secret generic sh-exchange-token -n "$NS" \
    --from-literal=SH_EXCHANGE_TOKEN="$(openssl rand -hex 32)" >/dev/null

kubectl apply -f control-plane.yaml >/dev/null
kubectl set env deploy/sh-control-plane -n "$NS" "SH_GITHUB_CLIENT_ID=$SH_GITHUB_CLIENT_ID" >/dev/null
kubectl rollout status deploy/sh-control-plane -n "$NS" --timeout=180s >/dev/null

echo "== switching the data plane to SH_REQUIRE_AUTH=true (the property, not the default)"
RESTORE_FLAG=1
set_ksvc_env SH_REQUIRE_AUTH=true "SH_SESSION_TOKEN_PUBLIC_KEYS=$PUBKEYS" >/dev/null

PF="$(ensure_port_forward || true)"
[ -n "$PF" ] && PFS+=("$PF")
kubectl port-forward -n "$NS" svc/sh-control-plane "${CP_PORT}:8080" >/dev/null 2>&1 &
PFS+=("$!")
sleep 3

cp_post() { curl -s -X POST -H 'Content-Type: application/json' -d "$2" "$CP_BASE$1"; }
cp_get() { curl -s -H "Authorization: Bearer $2" "$CP_BASE$1"; }
cp_code() {
  curl -s -o /dev/null -w '%{http_code}' -X "$1" -H "Authorization: Bearer $3" "$CP_BASE$2"
}

# --- log in as two humans ----------------------------------------------------------------------
# The device flow: the operator visits the URL and enters the code, first as Alice, then as Bob.
login() {
  local who="$1" start code uri dc token
  start="$(cp_post /v1/auth/device '{}')"
  dc="$(jq -r .deviceCode <<<"$start")"
  code="$(jq -r .userCode <<<"$start")"
  uri="$(jq -r .verificationUri <<<"$start")"
  echo "  ACTION: open $uri and enter code $code  --  signed in as $who" >&2
  for _ in $(seq 1 60); do
    local resp status
    resp="$(cp_post /v1/auth/device/token "$(jq -nc --arg d "$dc" '{deviceCode:$d}')")"
    token="$(jq -r '.token // empty' <<<"$resp")"
    [ -n "$token" ] && {
      printf '%s' "$token"
      return 0
    }
    status="$(jq -r '.error // empty' <<<"$resp")"
    [ "$status" = "authorization_pending" ] || {
      echo "  login failed for $who: $status" >&2
      return 1
    }
    sleep 5
  done
  echo "  login timed out for $who" >&2
  return 1
}

echo
echo "--- Claim 1: two GitHub logins yield two distinct, attested subjects ---"
ALICE_TOKEN="$(login Alice)"
BOB_TOKEN="$(login Bob)"
ALICE_SUB="$(jq -r .subject <<<"$(cp_get /v1/me "$ALICE_TOKEN")")"
BOB_SUB="$(jq -r .subject <<<"$(cp_get /v1/me "$BOB_TOKEN")")"
if [ -n "$ALICE_SUB" ] && [ -n "$BOB_SUB" ] && [ "$ALICE_SUB" != "$BOB_SUB" ]; then
  # The subject is github:<numeric id>, never the login: a login is mutable and reusable after account
  # deletion (spec §5.1).
  ok "$ALICE_SUB vs $BOB_SUB"
else
  ko "expected two distinct subjects, got '$ALICE_SUB' and '$BOB_SUB'"
fi

# --- each user stores their own credential -----------------------------------------------------
put_cred() {
  local token="$1" endpoint="$2"
  curl -s -o /dev/null -w '%{http_code}' -X PUT \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg t "${ANTHROPIC_AUTH_TOKEN:?ANTHROPIC_AUTH_TOKEN must be set}" --arg e "$endpoint" \
      '{kind:"bearer",consumer:"inference",destination:{hosts:["gateway"]},endpoint:$e,secret:{token:$t}}')" \
    "$CP_BASE/v1/credentials/my-inference"
}

echo
echo "--- Claim 2: each user stores a credential, and no route ever reads one back ---"
EP="${ANTHROPIC_BASE_URL:?ANTHROPIC_BASE_URL must be set}"
a_put="$(put_cred "$ALICE_TOKEN" "$EP")"
b_put="$(put_cred "$BOB_TOKEN" "$EP")"
listing="$(cp_get /v1/credentials "$ALICE_TOKEN")"
if [ "$a_put" = 204 ] && [ "$b_put" = 204 ] &&
  [ "$(jq -r '.credentials[0].name' <<<"$listing")" = my-inference ] &&
  ! jq -e '.credentials[0] | has("secret")' <<<"$listing" >/dev/null; then
  ok "stored; the listing carries metadata only"
else
  ko "credential write or metadata-only listing failed ($a_put/$b_put)"
fi

echo
echo "--- Claim 3: each user's session list contains only their own ---"
A_SID="$(jq -r .sessionId <<<"$(curl -s -X POST -H "Authorization: Bearer $ALICE_TOKEN" \
  -H 'Content-Type: application/json' -d '{}' "$CP_BASE/v1/sessions")")"
B_CREATE="$(curl -s -X POST -H "Authorization: Bearer $BOB_TOKEN" \
  -H 'Content-Type: application/json' -d '{}' "$CP_BASE/v1/sessions")"
B_SID="$(jq -r .sessionId <<<"$B_CREATE")"
B_SESSION_TOKEN="$(jq -r .token <<<"$B_CREATE")"
a_list="$(jq -r '[.sessions[].sessionId] | join(",")' <<<"$(cp_get /v1/sessions "$ALICE_TOKEN")")"
b_list="$(jq -r '[.sessions[].sessionId] | join(",")' <<<"$(cp_get /v1/sessions "$BOB_TOKEN")")"
if [ "$a_list" = "$A_SID" ] && [ "$b_list" = "$B_SID" ]; then
  ok "Alice sees $A_SID; Bob sees $B_SID"
else
  ko "list leaked across owners (alice='$a_list' bob='$b_list')"
fi

echo
echo "--- Claim 4: reading another user's session is 404, not 403 ---"
# 403 would be an existence oracle; 404 makes "someone else's" and "does not exist" identical (§8.1).
code="$(cp_code GET "/v1/sessions/$B_SID" "$ALICE_TOKEN")"
[ "$code" = 404 ] && ok "404" || ko "expected 404, got $code"

echo
echo "--- Claim 5: a valid token cannot drive another session ---"
A_SESSION_TOKEN="$(jq -r .token <<<"$(curl -s -X POST -H "Authorization: Bearer $ALICE_TOKEN" \
  -H 'Content-Type: application/json' -d "$(jq -nc --arg s "$A_SID" '{}')" "$CP_BASE/v1/sessions/$A_SID/token")")"
# shellcheck disable=SC2086
mismatch="$(curl -s $CURL_OPTS ${CURL_HDR[@]+"${CURL_HDR[@]}"} \
  -H "Authorization: Bearer $A_SESSION_TOKEN" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg s "$B_SID" '{sessionId:$s, prompt:"hello"}')" "$BASE/turn")"
[ "$(jq -r '.error // empty' <<<"$mismatch")" = session_mismatch ] &&
  ok "session_mismatch" || ko "expected session_mismatch, got $mismatch"

echo
echo "--- Claim 6: with SH_REQUIRE_AUTH=true an unauthenticated turn is 401 ---"
# shellcheck disable=SC2086
noauth="$(curl -s $CURL_OPTS ${CURL_HDR[@]+"${CURL_HDR[@]}"} -H 'Content-Type: application/json' \
  -d '{"prompt":"hello"}' "$BASE/turn")"
[ "$(jq -r '.error // empty' <<<"$noauth")" = token_required ] &&
  ok "token_required" || ko "expected token_required, got $noauth"

echo
echo "--- Claim 7: a turn runs on the subject's own resolved credential ---"
# shellcheck disable=SC2086
turn="$(curl -s $CURL_OPTS --max-time 120 ${CURL_HDR[@]+"${CURL_HDR[@]}"} \
  -H "Authorization: Bearer $B_SESSION_TOKEN" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg s "$B_SID" '{sessionId:$s, prompt:"Reply with exactly: MULTIUSER-OK"}')" \
  "$BASE/turn")"
grep -q 'MULTIUSER-OK' <<<"$turn" && ok "Bob's turn completed" || ko "turn failed: $turn"

echo
echo "--- Claim 8: /resources reports where the session ran ---"
res="$(cp_get "/v1/sessions/$B_SID/resources" "$BOB_TOKEN")"
if jq -e '.session.id and .harness and .sandbox' <<<"$res" >/dev/null; then
  ok "harness=$(jq -r '.harness.podName // "none"' <<<"$res") sandbox=$(jq -r '.sandbox.phase' <<<"$res")"
else
  ko "unexpected /resources shape: $res"
fi

echo
echo "--- Claim 9: no credential, no session -- with the deployment's own key in the environment ---"
# The load-bearing claim (spec §9.3 test 1). ANTHROPIC_AUTH_TOKEN is mounted on the ksvc from
# llm-credentials, and it is still not what a session runs on.
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $ALICE_TOKEN" \
  "$CP_BASE/v1/credentials/my-inference"
denied="$(curl -s -X POST -H "Authorization: Bearer $ALICE_TOKEN" \
  -H 'Content-Type: application/json' -d '{}' "$CP_BASE/v1/sessions")"
if [ "$(jq -r '.error // empty' <<<"$denied")" = credential_required ]; then
  ok "credential_required, though the deployment's key is present in the harness environment"
else
  ko "expected credential_required, got $denied"
fi
put_cred "$ALICE_TOKEN" "$EP" >/dev/null # restore, so a re-run starts from a working state

echo
echo "--- Claim 10: deleting a session removes it from its owner's list ---"
del="$(cp_code DELETE "/v1/sessions/$A_SID" "$ALICE_TOKEN")"
after="$(jq -r '[.sessions[].sessionId] | join(",")' <<<"$(cp_get /v1/sessions "$ALICE_TOKEN")")"
if { [ "$del" = 204 ] || [ "$del" = 202 ]; } && [ "$after" != "$A_SID" ]; then
  ok "delete returned $del and the session is gone from the list"
else
  ko "delete=$del list='$after'"
fi

echo
echo "NOTE: slice 1's sandbox pool is SHARED. Two users' leaves can be placed on the same pod;"
echo "      isolation here holds at the API, the session store and the inference credential, not the"
echo "      sandbox. The tenant-labelled partition is MU2 (spec §8.2)."
echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
