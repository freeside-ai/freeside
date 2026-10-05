#!/bin/sh
# Test-only stdio driver. All raw responses stay in this disposable container.
# The outer invocation bounds the session, including startup, at 60 seconds.
set -eu
umask 077

if [ "${1:-}" = session ]; then
    mkfifo /capture/in /capture/raw /capture/out
    exec 3<>/capture/in
    proxy=$(jq -r .proxy /probe/config.json)
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
    printf '%s\n' '{"id":1,"method":"initialize","params":{"clientInfo":{"name":"freeside_account_probe","version":"1"}}}' >&3
    next=0
    count=$(jq '.requests | length' /probe/config.json)
    while IFS= read -r line <&4; do
        printf '%s\n' "$line" > /capture/message.json
        if ! jq -e 'type == "object"' /capture/message.json >/dev/null 2>&1; then
            touch /capture/malformed
            exit 1
        fi
        id=$(jq -r '.id // "notification"' /capture/message.json)
        if [ "$id" = 1 ]; then
            if ! jq -e 'has("result") and (has("error") | not)' /capture/message.json >/dev/null; then
                touch /capture/rpcError
                exit 1
            fi
            touch /capture/initialized
            printf '%s\n' '{"method":"initialized"}' >&3
        elif [ "$id" = "$((next+1))" ] && [ "$next" -gt 0 ]; then
            touch /capture/answered
            if jq -e 'has("error")' /capture/message.json >/dev/null; then
                touch /capture/rpcError
            elif ! jq '.result' /capture/message.json | jq -ef /probe/sanitize.jq > /capture/account.json 2>/dev/null; then
                touch /capture/malformed
                exit 1
            fi
        else
            continue
        fi
        if [ "$next" -eq "$count" ]; then exit 0; fi
        jq -c --argjson n "$next" '.requests[$n] + {id:($n+2)}' /probe/config.json >&3
        next=$((next+1))
    done
    exit 1
fi

mkdir -p /capture /var/lib/freeside/home /var/lib/freeside/codex-home
auth=/var/lib/freeside/codex-snapshot/auth.json
link=/var/lib/freeside/codex-home/auth.json
ln -s "$auth" "$link"
before=$(sha256sum "$auth" | cut -d ' ' -f1)
version=$(codex --version 2>/dev/null)
if [ "$version" = 'codex-cli 0.147.0' ]; then
    version=0.147.0
else
    version=unexpected
fi
status=0
timeout -k 2 60 sh /probe/probe.sh session || status=$?
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
if [ ! -s /capture/account.json ]; then
    printf '%s\n' '{"account":false,"fields":{},"plan":""}' > /capture/account.json
fi
jq -n --arg version "$version" --slurpfile account /capture/account.json \
    --argjson initialized "$(test -f /capture/initialized && echo true || echo false)" \
    --argjson answered "$(test -f /capture/answered && echo true || echo false)" \
    --argjson rpcError "$(test -f /capture/rpcError && echo true || echo false)" \
    --argjson malformed "$(test -f /capture/malformed && echo true || echo false)" \
    --argjson timedOut "$timedOut" --argjson overflow "$overflow" --argjson driverFailed "$driverFailed" \
    --argjson authUnchanged "$unchanged" --argjson symlinkSame "$symlink" \
    --argjson appendDenied "$append" --argjson unlinkDenied "$unlink" \
    '$ARGS.named + $account[0] | del(.account) + {account:$account[0].account}' > /capture/result.json
