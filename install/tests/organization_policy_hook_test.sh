#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
installer="${INSTALLER:-$script_dir/../install.sh}"
uninstaller="${UNINSTALLER:-$script_dir/../uninstall.sh}"
work="$(mktemp -d)"
server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

fake_bin="$work/bin"
home="$work/home with spaces"
mkdir -p "$fake_bin" "$home/.claude"
printf '%s\n' '#!/usr/bin/env bash' 'exit 22' >"$fake_bin/curl"
chmod +x "$fake_bin/curl"
printf '%s\n' '{"env":{"WEAVE_POLICY_API_URL":"https://app.workweave.ai/api/weave_router/organization-policy"},"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"user-hook"}]}]}}' >"$home/.claude/settings.json"

HOME="$home" XDG_CACHE_HOME="$work/home/.cache" PATH="$fake_bin:$PATH" \
  NO_COLOR=1 WEAVE_ROUTER_KEY="rk_policy_test" \
  bash "$installer" --claude --quiet --non-interactive --scope user \
    --base-url http://127.0.0.1:9 </dev/null >/dev/null 2>&1

settings="$home/.claude/settings.json"
hook="$home/.claude/weave-router-policy.js"
printf -v hook_command '%q %q' "$(command -v node)" "$hook"
test -f "$hook"
jq -e ' .env.WEAVE_POLICY_API_URL == null ' "$settings" >/dev/null
jq -e --arg hook_command "$hook_command" '
  any(.hooks.SessionStart[]?.hooks[]?; .command == $hook_command) and
  any(.hooks.SubagentStart[]?.hooks[]?; .command == $hook_command) and
  any(.hooks.SessionStart[]?.hooks[]?; .command == "user-hook") and
  ([.hooks.UserPromptSubmit[]?.hooks[]?] | length == 0)
' "$settings" >/dev/null

cat >"$work/server.js" <<'NODE'
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const content = "Use the organization's documented workflow.";
const countFile = process.argv[4];
const server = http.createServer((request, response) => {
  fs.appendFileSync(countFile, "request\n");
  if (request.headers["x-weave-router-key"] !== "rk_policy_test") {
    response.writeHead(401).end();
    return;
  }
  const policy = JSON.stringify({
    hash: crypto.createHash("sha256").update(content).digest("hex"),
    revision: 7,
    audience: fs.readFileSync(process.argv[3], "utf8"),
    content,
  });
  response.writeHead(200, {"content-type": "application/json"}).end(policy);
});
server.listen(0, "127.0.0.1", () => fs.writeFileSync(process.argv[2], String(server.address().port)));
NODE
port_file="$work/port"
printf '%s' main_and_subagents >"$work/audience"
: >"$work/requests"
node "$work/server.js" "$port_file" "$work/audience" "$work/requests" &
server_pid=$!
for _ in $(seq 1 100); do
  [ -s "$port_file" ] && break
  sleep 0.02
done
test -s "$port_file"

policy_endpoint="http://127.0.0.1:$(cat "$port_file")"
headers="$(jq -r '.env.ANTHROPIC_CUSTOM_HEADERS' "$settings")"
run_hook() {
  local event="$1" source="${2:-}"
  printf '{"hook_event_name":"%s","source":"%s"}\n' "$event" "$source" |
    ANTHROPIC_CUSTOM_HEADERS="$headers" WEAVE_POLICY_API_URL="$policy_endpoint" bash -c "$hook_command"
}

startup="$(run_hook SessionStart startup)"
[[ "$startup" == *'"hookEventName":"SessionStart"'* ]]
[[ "$startup" == *"documented workflow."* ]]
test "$(wc -l <"$work/requests" | tr -d ' ')" = 1
test -z "$(run_hook SessionStart resume)"
test "$(wc -l <"$work/requests" | tr -d ' ')" = 1
compaction="$(run_hook SessionStart compact)"
[[ "$compaction" == *'"hookEventName":"SessionStart"'* ]]
child="$(run_hook SubagentStart)"
[[ "$child" == *'"hookEventName":"SubagentStart"'* ]]
printf '%s' main_thread >"$work/audience"
test -z "$(run_hook SubagentStart)"
printf '%s' main_and_subagents >"$work/audience"

HOME="$home" XDG_CACHE_HOME="$work/home/.cache" \
  bash "$uninstaller" --claude --scope user </dev/null >/dev/null 2>&1
test ! -e "$hook"
jq -e --arg hook_command "$hook_command" '
  ([.hooks.SessionStart[]?.hooks[]? | select(.command == "user-hook")] | length) == 1 and
  ([.hooks.SessionStart[]?.hooks[]? | select(.command == $hook_command)] | length) == 0 and
  (.env.WEAVE_POLICY_API_URL == null)
' "$settings" >/dev/null

echo "Organization policy hook lifecycle tests passed"
