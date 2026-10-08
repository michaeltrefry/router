#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
installer="${INSTALLER:-$script_dir/../install.sh}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/home"
export FAKE_CURL_LOG="$work/curl.log"
export FAKE_ENROLLMENTS="$work/enrollments.jsonl"

payload="$(printf '%s' '{"chatgpt_account_id":"chatgpt-test","chatgpt_user_id":"codex-user-test","email":"codex@example.test","organizations":[{"id":"org-test","name":"Codex Org"}]}' | openssl base64 -A | tr '+/' '-_' | tr -d '=')"
export FAKE_JWT="eyJhbGciOiJSUzI1NiJ9.$payload.signature"

cat >"$work/bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
out=""
data_file=""
url=""
want_status="false"
user_agent=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -A) user_agent="$2"; shift 2 ;;
    -w) want_status="true"; shift 2 ;;
    --data-binary)
      data_file="${2#@}"
      case "$2" in *refresh-*|*access-*|*authorization-code*|*pkce-verifier*) exit 91 ;; esac
      shift 2
      ;;
    -H|--header|-X|--max-time) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\t%s\t%s\n' "$url" "$data_file" "$user_agent" >>"$FAKE_CURL_LOG"
case "$url" in
  */api/accounts/deviceauth/usercode)
    printf '%s' '{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"1"}'
    ;;
  */api/accounts/deviceauth/token)
    printf '%s' '{"authorization_code":"authorization-code","code_verifier":"pkce-verifier"}'
    ;;
  */v1/oauth/token)
    printf '%s' "$FAKE_CLAUDE_RESPONSE"
    ;;
  */oauth/token)
    printf '{"id_token":"%s","access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}' "$FAKE_JWT"
    ;;
  */validate)
    printf '%s' '{}' >"$out"
    [ "$want_status" = "true" ] && printf '200'
    ;;
  */v1/subscriptions/accounts)
    if [ -n "${FAKE_ACCOUNTS_STATUS:-}" ]; then
      printf '%s' "${FAKE_ACCOUNTS_BODY:-}" >"$out"
      [ "$want_status" = "true" ] && printf '%s' "$FAKE_ACCOUNTS_STATUS"
    elif [ -n "$data_file" ]; then
      jq -e '.refresh_token | startswith("refresh-")' "$data_file" >/dev/null
      jq -c . "$data_file" >>"$FAKE_ENROLLMENTS"
      jq '{id:"opaque-1",provider,external_account_id,display_name,enabled:true}' "$data_file" >"$out"
      [ "$want_status" = "true" ] && printf '201'
    else
      printf '%s' '[{"id":"opaque-1","provider":"codex","external_account_id":"chatgpt-test","display_name":"Codex Org: codex@example.test","enabled":true}]' >"$out"
      [ "$want_status" = "true" ] && printf '200'
    fi
    ;;
  *) exit 22 ;;
esac
FAKE_CURL
chmod +x "$work/bin/curl"
browser_log="$work/browser.log"
: >"$browser_log"
for opener in open xdg-open; do
  printf '%s\n' '#!/usr/bin/env bash' "printf '%s\\n' \"\$*\" >>\"$browser_log\"" >"$work/bin/$opener"
  chmod +x "$work/bin/$opener"
done

common_env=(HOME="$work/home" PATH="$work/bin:$PATH" WEAVE_ROUTER_KEY="rk_test_secret" NO_COLOR=1)
env "${common_env[@]}" bash "$installer" login codex --base-url https://router.example.test --non-interactive --quiet \
  | grep -Fq 'Codex subscription enrolled.'
if [ -s "$browser_log" ]; then
  echo "non-interactive login launched a browser: $(cat "$browser_log")" >&2
  exit 1
fi

# Login is interactive for OAuth, but it does not install a client config and
# therefore must not ask the unrelated user-vs-project scope question. Run it
# under a real TTY so the regression catches the prompt that a pipe would hide.
env "${common_env[@]}" python3 - "$installer" <<'PY'
import os
import pty
import select
import sys
import time

installer = sys.argv[1]
pid, fd = pty.fork()
if pid == 0:
    os.execvpe("bash", ["bash", installer, "login", "codex", "--base-url", "https://router.example.test", "--quiet"], os.environ)

output = bytearray()
deadline = time.monotonic() + 30
while True:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        os.kill(pid, 9)
        raise SystemExit("timed out waiting for interactive login")
    ready, _, _ = select.select([fd], [], [], remaining)
    if not ready:
        continue
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    output.extend(chunk)
    # Keep the regression test from hanging if the old scope prompt is
    # reintroduced; the assertion below still reports that it appeared.
    if b"Install scope:" in output:
        os.write(fd, b"\n")

_, status = os.waitpid(pid, 0)
if b"Install scope:" in output:
    sys.stderr.write(output.decode(errors="replace"))
    raise SystemExit("login unexpectedly prompted for install scope")
if b"Codex subscription enrolled." not in output:
    sys.stderr.write(output.decode(errors="replace"))
    raise SystemExit("interactive login did not complete")
if not os.WIFEXITED(status) or os.WEXITSTATUS(status) != 0:
    raise SystemExit("interactive login failed")
PY
grep -Fq '/codex/device' "$browser_log" || { echo 'interactive login did not open the device URL' >&2; exit 1; }

env "${common_env[@]}" python3 "$script_dir/claude_login_test.py" "$installer"

status_output="$(env "${common_env[@]}" bash "$installer" status --base-url https://router.example.test --quiet)"
grep -Fq 'Identity: rk_…cret' <<<"$status_output"
grep -Fq 'Connectivity: connected' <<<"$status_output"
grep -Fq 'codex  chatgpt-test  [Codex Org: codex@example.test]  enabled  ready' <<<"$status_output"
grep -Fq 'Codex Org: codex@example.test' <<<"$status_output"
if grep -Eq 'refresh-|access-' "$FAKE_CURL_LOG"; then
  echo 'refresh token leaked into curl argv log' >&2
  exit 1
fi

# OAuth issuers rate-limit curl's default user agent, so every token-endpoint
# call must identify the installer.
while IFS=$'\t' read -r logged_url _ logged_agent; do
  case "$logged_url" in
    *deviceauth*|*/oauth/token)
      if [ -z "$logged_agent" ]; then
        echo "OAuth call to $logged_url sent no explicit user agent" >&2
        exit 1
      fi
      ;;
  esac
done <"$FAKE_CURL_LOG"

# A router without server-side pools does not mount /v1/subscriptions; the
# failure must say so, not point at the unrelated model-selection API.
if pools_off="$(env "${common_env[@]}" FAKE_ACCOUNTS_STATUS=404 bash "$installer" login codex --base-url https://router.example.test --non-interactive --quiet 2>&1)"; then
  echo 'login succeeded against a router without subscription pools' >&2
  exit 1
fi
grep -Fq 'ROUTER_SUBSCRIPTION_POOLS_ENABLED' <<<"$pools_off"
if grep -Fq '/admin/v1/models' <<<"$pools_off"; then
  echo 'login 404 still blames the model-selection API' >&2
  exit 1
fi
accounts_off="$(env "${common_env[@]}" FAKE_ACCOUNTS_STATUS=404 bash "$installer" accounts --base-url https://router.example.test --quiet 2>&1 || true)"
grep -Fq 'ROUTER_SUBSCRIPTION_POOLS_ENABLED' <<<"$accounts_off"

# A shared installation key cannot own subscriptions; say which key is needed.
if shared_key="$(env "${common_env[@]}" FAKE_ACCOUNTS_STATUS=503 FAKE_ACCOUNTS_BODY='{"error":"subscription_owner_unavailable"}' bash "$installer" login codex --base-url https://router.example.test --non-interactive --quiet 2>&1)"; then
  echo 'login succeeded with a shared installation key' >&2
  exit 1
fi
grep -Fq 'personal router key' <<<"$shared_key"
grep -Fq 'make personal-key' <<<"$shared_key"

echo "Subscription CLI regression tests passed"
