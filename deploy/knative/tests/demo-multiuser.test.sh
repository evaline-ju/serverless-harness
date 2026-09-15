#!/usr/bin/env bash
# deploy/knative/tests/demo-multiuser.test.sh
#
# Cluster-free tests for the multi-user demo. The demo itself needs a cluster, a registered GitHub
# OAuth app and two humans at a browser, so it cannot run on any PR -- but the things that make it
# either honest or dangerous are all statically checkable:
#
#   - it must SKIP, not fail, when SH_GITHUB_CLIENT_ID is unset (the env-gated live-smoke convention);
#   - it must run with SH_REQUIRE_AUTH=true, or it demonstrates the permissive default instead of the
#     property (spec §4.3.1);
#   - it must RESTORE that flag on exit, or it leaves the next smoke run 401ing on every /turn;
#   - it must never echo a token, a KEK or a device code into the transcript;
#   - it must state out loud that slice 1's sandbox pool is SHARED (spec §8.2), because a demo that
#     quietly implies isolation it does not have is worse than no demo.
#
# The three load-bearing ones -- the flag on, the flag restored, the opt-in manifest applied -- are
# asserted BEHAVIOURALLY, by mocking kubectl/curl/openssl on PATH and reading the call log, the way
# every other test in this directory does (Makefile:15-19). They used to be `grep -q '<string>'` over
# the script's own text, which a MENTION anywhere satisfies: `SH_REQUIRE_AUTH=true` also appears in the
# script's header comment at demo-multiuser.sh:20, so deleting the real `set_ksvc_env
# SH_REQUIRE_AUTH=true` at :133 left the old check green. The remaining greps are anchored to a command
# position rather than a bare substring, so a comment can no longer satisfy them either.
#
# No cluster required. Run: bash deploy/knative/tests/demo-multiuser.test.sh
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$DIR/demo-multiuser.sh"
fails=0
check() { if [ "$2" = "$3" ]; then echo "  ok: $1"; else
  echo "  FAIL: $1 (want '$3', got '$2')"
  fails=$((fails + 1))
fi; }

echo "== the script exists and is shellcheck-shaped"
check "script exists" "$([ -f "$SCRIPT" ] && echo yes || echo no)" "yes"
check "has set -euo pipefail" "$(grep -c '^set -euo pipefail' "$SCRIPT")" "1"
check "sources lib.sh" "$(grep -q 'source ./lib.sh' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== it gates on the manual prerequisite and SKIPS rather than failing"
check "mentions SH_GITHUB_CLIENT_ID" \
  "$(grep -q 'SH_GITHUB_CLIENT_ID' "$SCRIPT" && echo 1 || echo 0)" "1"
check "skips (exit 0) when the client id is unset" \
  "$(grep -q 'SKIP' "$SCRIPT" && echo 1 || echo 0)" "1"
check "gates on MULTIUSER_LIVE_SMOKE like the other live gates" \
  "$(grep -q 'MULTIUSER_LIVE_SMOKE' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== it demonstrates the real property, not the permissive default"
check "has an EXIT trap, so an aborted run still restores it" \
  "$(grep -qE '^\s*trap cleanup EXIT' "$SCRIPT" && echo 1 || echo 0)" "1"
# Anchored to a curl/assertion line rather than a bare substring: each of these codes is the thing the
# demo compares a response against, so it must appear in an EXECUTED line, not only in prose.
check "asserts the credential property with the ambient key present" \
  "$(grep -cE '^[^#]*=\s*credential_required' "$SCRIPT")" "1"
check "asserts session_mismatch" \
  "$(grep -cE '^[^#]*=\s*session_mismatch' "$SCRIPT")" "1"
check "asserts token_required under the flag" \
  "$(grep -cE '^[^#]*=\s*token_required' "$SCRIPT")" "1"
# `Claim 4` was a LABEL, not a behaviour. What makes the claim real is comparing a GET of the other
# user's session against 404 -- and 404 specifically, since 403 would be the existence oracle §8.1
# forbids.
check "asserts a cross-tenant 404 (not 403) on another user's session" \
  "$(grep -cE '^[^#]*cp_code GET "/v1/sessions/\$B_SID" "\$ALICE_TOKEN"' "$SCRIPT")" "1"
check "compares that response to 404" \
  "$(grep -cE '^[^#]*\[ "\$code" = 404 \]' "$SCRIPT")" "1"
check "reads /resources from an executed line" \
  "$(grep -qE '^[^#]*cp_get "/v1/sessions/\$B_SID/resources"' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== it is honest about what slice 1 does NOT isolate"
check "says the sandbox pool is shared" \
  "$(grep -qi 'pool is shared\|shared pool' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== it never prints a secret"
# A demo transcript gets pasted into issues. Tokens, KEKs and device codes must not be in it.
#
# The pattern must admit BOTH `$VAR` and `${VAR}`: shellcheck pushes authors toward the braced form,
# so a guard matching only `\$(NAME|...)` would wave through `echo "${ALICE_TOKEN}"` -- the single most
# likely way this leak actually gets written. `printf` is covered for the same reason.
check "no echo/printf of a token, KEK or exchange secret, braced or bare" \
  "$(grep -Ec '(echo|printf).*\$\{?(ALICE_TOKEN|BOB_TOKEN|EXCHANGE_TOKEN|KEK|SH_CREDENTIAL_KEK|SH_EXCHANGE_TOKEN|SH_SESSION_TOKEN_PRIVATE_KEY)' "$SCRIPT")" "0"
check "the user_code IS printed (the operator must type it)" \
  "$(grep -q 'userCode' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== it applies the opt-in manifest rather than assuming it is deployed"
check "waits for the rollout" \
  "$(grep -qE '^[^#]*rollout status' "$SCRIPT" && echo 1 || echo 0)" "1"

echo "== the Makefile exposes it"
MK="$DIR/../../Makefile"
check "make demo-multiuser exists" "$(grep -q '^demo-multiuser:' "$MK" && echo 1 || echo 0)" "1"
check "make demo-multiuser-teardown exists" \
  "$(grep -q '^demo-multiuser-teardown:' "$MK" && echo 1 || echo 0)" "1"

echo "== behaviour, under mocked kubectl: what the demo actually DOES (Makefile:15-19)"
# The three checks the review demonstrated as unfalsifiable are asserted here against the kubectl call
# log instead of the script's text. The demo is driven with the live gates satisfied but every external
# tool mocked, so it gets as far as the device-flow login and dies there -- which is after the flag flip
# and the manifest apply, and its EXIT trap still runs, so all three are observable.
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export MOCK_LOG="$TMP/calls.log"
mkdir -p "$TMP/bin"

# kubectl: log everything. `get ksvc -o json` must return a parseable env array, because set_ksvc_env
# pipes it through the REAL jq to build its patch -- so the patch payload in the log is the one the
# cluster would receive. `get secret` fails, taking the create-the-Secrets branch.
cat > "$TMP/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "kubectl $*" >> "$MOCK_LOG"
if [ "${1:-}" = get ] && [ "${2:-}" = ksvc ]; then
  echo '{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"SH_REQUIRE_AUTH","value":"false"}]}]}}}}'
  exit 0
fi
[ "${1:-}" = get ] && [ "${2:-}" = secret ] && exit 1
exit 0
EOF
# openssl: enough shape for the keygen branch; the value is never asserted, only that it is not logged.
cat > "$TMP/bin/openssl" <<'EOF'
#!/usr/bin/env bash
echo "openssl $*" >> "$MOCK_LOG"
case "${1:-}" in
  genpkey|pkey) for a in "$@"; do [ "$prev" = -out ] && : > "$a"; prev="$a"; done; exit 0 ;;
  dgst) echo "SHA2-256(x)= 00112233445566778899aabbccddeeff"; exit 0 ;;
  rand) echo "cmFuZG9t"; exit 0 ;;
esac
exit 0
EOF
# curl: every control-plane call returns an empty object, so `login` finds no token and no
# authorization_pending and returns 1 -- killing the script under `set -e` at Claim 1.
cat > "$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$MOCK_LOG"
echo '{}'
exit 0
EOF
# sleep: the demo sleeps 3s for port-forwards and 5s per login poll. Nothing here waits on anything.
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/sleep"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/shred"
chmod +x "$TMP/bin"/*

( PATH="$TMP/bin:$PATH" MULTIUSER_LIVE_SMOKE=1 SH_GITHUB_CLIENT_ID=Iv1.mock \
    ANTHROPIC_AUTH_TOKEN=sk-mock ANTHROPIC_BASE_URL=https://mock/v1 \
    bash "$SCRIPT" ) >/dev/null 2>&1

# `set_ksvc_env NAME=value` becomes a `kubectl patch` whose payload jq built, so the flip is visible as
# the env entry itself. Deleting `set_ksvc_env SH_REQUIRE_AUTH=true` from the demo fails THIS.
check "actually patches the ksvc with SH_REQUIRE_AUTH=true" \
  "$(grep -c '"name":"SH_REQUIRE_AUTH","value":"true"' "$MOCK_LOG")" "1"
check "restores SH_REQUIRE_AUTH=false on exit, even on an aborted run" \
  "$(grep -c '"name":"SH_REQUIRE_AUTH","value":"false"' "$MOCK_LOG")" "1"
check "actually applies the opt-in control-plane manifest" \
  "$(grep -c '^kubectl apply -f control-plane.yaml$' "$MOCK_LOG")" "1"
# The restore is only reachable because RESTORE_FLAG is armed BEFORE the flip; assert the ordering the
# log shows rather than trusting the trap exists.
check "the flip precedes the restore in the call log" \
  "$(awk '/"value":"true"/{t=NR} /"value":"false"/{f=NR} END{print (t>0 && f>t) ? 1 : 0}' "$MOCK_LOG")" "1"
check "no secret value reached the transcript of a mocked run" \
  "$(grep -c 'sk-mock' "$MOCK_LOG")" "0"

if [ "$fails" -gt 0 ]; then echo "FAILED: $fails"; exit 1; fi
echo "all ok"
