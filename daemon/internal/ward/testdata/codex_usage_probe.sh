#!/bin/sh
# Test-only stdio driver. All raw responses stay in this disposable container.
# The outer invocation bounds the session, including startup, at the deadline
# the host pins in config.json. Its one argument is the host clock, in epoch
# seconds, at the last launch-gate check.
set -eu
umask 077

elapsed() { echo $(($(date +%s) - $1)); }

if [ "${1:-}" = session ]; then
    mkfifo /capture/in /capture/raw /capture/out
    exec 3<>/capture/in
    proxy=$(jq -r .proxy /probe/config.json)
    mode=$(jq -r .mode /probe/config.json)
    started=$(date +%s)
    env -i PATH="$PATH" HOME=/var/lib/freeside/home CODEX_HOME=/var/lib/freeside/codex-home \
        HTTP_PROXY="$proxy" HTTPS_PROXY="$proxy" http_proxy="$proxy" https_proxy="$proxy" \
        NO_PROXY= no_proxy= CODEX_CA_CERTIFICATE=/probe/ca.pem \
        codex app-server </capture/in >/capture/raw 2>/dev/null &
    server=$!
    # One extra byte is enough to prove overflow without retaining an unbounded
    # stream. The raw copy remains private even when JSON decoding fails.
    (dd bs=1 count=1048577 status=none </capture/raw | tee /capture/raw.jsonl >/capture/out) &
    reader=$!
    trap 'kill "$server" "$reader" 2>/dev/null || :; wait "$server" "$reader" 2>/dev/null || :' EXIT
    exec 4</capture/out
    printf '%s\n' '{"id":1,"method":"initialize","params":{"clientInfo":{"name":"freeside_usage_probe","version":"1"}}}' >&3
    while IFS= read -r line <&4; do
        printf '%s\n' "$line" > /capture/message.json
        if ! jq -e 'type == "object"' /capture/message.json >/dev/null 2>&1; then
            touch /capture/malformed
            exit 1
        fi
        # A server request carries both a method and an id. Nothing here answers
        # one; the thread asks for no approvals, so none is expected.
        kind=$(jq -r 'if has("method") then (if has("id") then "request" else .method end) else "response:\(.id)" end' /capture/message.json)
        case "$kind" in
        response:1)
            if ! jq -e 'has("result") and (has("error") | not)' /capture/message.json >/dev/null; then
                touch /capture/rpcError
                exit 1
            fi
            touch /capture/initialized
            elapsed "$started" > /capture/initSeconds
            printf '%s\n' '{"method":"initialized"}' >&3
            printf '%s\n' '{"id":2,"method":"account/rateLimits/read"}' >&3
            ;;
        response:2)
            touch /capture/answered
            elapsed "$started" > /capture/readSeconds
            if jq -e 'has("error")' /capture/message.json >/dev/null; then
                touch /capture/rpcError
                exit 0
            fi
            if ! jq '.result' /capture/message.json | jq -e --arg kind read -f /probe/sanitize.jq > /capture/usage.json 2>/dev/null; then
                touch /capture/malformed
                exit 1
            fi
            if [ "$mode" != turn ]; then exit 0; fi
            printf '%s\n' '{"id":3,"method":"thread/start","params":{"approvalPolicy":"never","sandbox":"read-only","cwd":"/var/lib/freeside/home","ephemeral":true}}' >&3
            ;;
        response:3)
            if ! jq -e 'has("result") and (.result.thread.id | type == "string")' /capture/message.json >/dev/null 2>&1; then
                touch /capture/rpcError
                exit 0
            fi
            jq -c '{id:4,method:"turn/start",params:{threadId:.result.thread.id,effort:"low",input:[{type:"text",text:"Reply with the single word ok."}]}}' /capture/message.json >&3
            ;;
        response:4)
            if jq -e 'has("error")' /capture/message.json >/dev/null; then
                touch /capture/rpcError
                exit 0
            fi
            touch /capture/turnStarted
            ;;
        account/rateLimits/updated)
            if ! jq '.params' /capture/message.json | jq -ce --arg kind updated -f /probe/sanitize.jq >> /capture/updated.jsonl 2>/dev/null; then
                touch /capture/malformed
                exit 1
            fi
            ;;
        turn/completed)
            touch /capture/turnCompleted
            if ! jq -e '.params.turn.status == "completed"' /capture/message.json >/dev/null 2>&1; then
                touch /capture/turnFailed
            fi
            exit 0
            ;;
        *) continue ;;
        esac
    done
    exit 1
fi

flag() { if [ -f "/capture/$1" ]; then echo true; else echo false; fi; }
seconds() { if [ -s "/capture/$1" ]; then cat "/capture/$1"; else echo 0; fi; }

mkdir -p /capture /var/lib/freeside/home /var/lib/freeside/codex-home
auth=/var/lib/freeside/codex-snapshot/auth.json
link=/var/lib/freeside/codex-home/auth.json
ln -s "$auth" "$link"
before=$(sha256sum "$auth" | cut -d ' ' -f1)
mode=$(jq -r .mode /probe/config.json)
deadline=$(jq -r .deadline /probe/config.json)
case "$mode" in idle | turn) ;; *) exit 1 ;; esac
case "$deadline" in '' | *[!0-9]*) exit 1 ;; esac
gate=${1:-}
case "$gate" in '' | *[!0-9]*) exit 1 ;; esac
version=$(codex --version 2>/dev/null)
if [ "$version" = 'codex-cli 0.147.0' ]; then
    version=0.147.0
else
    version=unexpected
fi
status=0
launched=$(date +%s)
timeout -k 2 "$deadline" sh /probe/probe.sh session || status=$?
total=$(elapsed "$launched")
timedOut=false
driverFailed=false
if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then timedOut=true
elif [ "$status" -ne 0 ]; then driverFailed=true; fi
overflow=false
if [ -f /capture/raw.jsonl ] && [ "$(wc -c < /capture/raw.jsonl)" -gt 1048576 ]; then overflow=true; fi
unchanged=false
if [ "$(sha256sum "$auth" | cut -d ' ' -f1)" = "$before" ]; then unchanged=true; fi
symlink=false
if [ -L "$link" ] && [ "$(readlink "$link")" = "$auth" ]; then symlink=true; fi
append=false
unlink=false
if ! (printf x >> "$auth") 2>/dev/null; then append=true; fi
if ! rm "$auth" 2>/dev/null; then unlink=true; fi
if [ ! -s /capture/usage.json ]; then
    printf '%s\n' '{"usage":false,"fields":{},"buckets":0,"codexBucket":false,"plan":""}' > /capture/usage.json
fi
touch /capture/updated.jsonl
jq -s --arg kind merge -f /probe/sanitize.jq /capture/updated.jsonl > /capture/updates.json
jq -n --arg version "$version" --arg mode "$mode" \
    --slurpfile usage /capture/usage.json --slurpfile updates /capture/updates.json \
    --argjson initialized "$(flag initialized)" --argjson answered "$(flag answered)" \
    --argjson turnStarted "$(flag turnStarted)" --argjson turnCompleted "$(flag turnCompleted)" \
    --argjson turnFailed "$(flag turnFailed)" \
    --argjson initSeconds "$(seconds initSeconds)" --argjson readSeconds "$(seconds readSeconds)" \
    --argjson totalSeconds "$total" --argjson launchSeconds "$((launched - gate))" \
    --argjson rpcError "$(flag rpcError)" --argjson malformed "$(flag malformed)" \
    --argjson timedOut "$timedOut" --argjson overflow "$overflow" --argjson driverFailed "$driverFailed" \
    --argjson authUnchanged "$unchanged" --argjson symlinkSame "$symlink" \
    --argjson appendDenied "$append" --argjson unlinkDenied "$unlink" \
    '($ARGS.named | del(.usage, .updates)) + $usage[0] + $updates[0]' > /capture/result.json
