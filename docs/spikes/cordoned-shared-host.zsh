#!/bin/zsh
# Shared-host cordon spike (#3596): run the cordon on a host the daemon
# doesn't run on, as the standard account `agent` that the owner's own
# unconfined sessions share. Method and results:
# docs/spikes/cordoned-shared-host.md.
#
# Usage: zsh cordoned-shared-host.zsh SUBCOMMAND [args]
#   header | item1-helper | item1-remote USER@HOST | item2 [console|ssh] |
#   item3 N LABEL | summarize3 LABEL | load-start | load-stop |
#   item4-build VERSION | item4-swap VERSION | item5-build | item5 LABEL |
#   item5-prompt | item6 | item7 | item8 | item9 LABEL | item9-warn |
#   claude-dirs | cleanup
# Env: ROOT (spike root, made with mktemp -d outside any repository and
#   resolved through /private/tmp), REPO (the Freeside worktree the spike's
#   branch is checked out in).
#
# Nothing here reads a provider token. Every launch runs the cordon
# package's stand-in agent, or a plain zsh in its place, with the package's
# fixture token. Records print counts and outcomes, never a value read from
# a secret path. The suite's environment check fails on any variable whose
# name looks like a credential, so each test run starts with them unset.
set -u
setopt extendedglob nullglob
: ${ROOT:?} ${REPO:?}
HERE=${0:A:h}
TESTFILE=$REPO/daemon/internal/cordon/shared_host_spike_test.go
mkdir -p $ROOT/scripts
log() { echo "$(date -u +%FT%TZ) $*" | tee -a $ROOT/events.log; }

# The suite's environment check refuses a credential-named variable.
scrub_env() {
  local v
  for v in ${(k)parameters}; do
    [[ $v == (#i)*(token|secret|passw|credential|api_key|apikey|access_key|private_key)* ]] && unset $v 2>/dev/null
  done
  return 0
}

# Runs the throwaway test (cordoned-shared-host-cordon_test.go.txt) from the
# cordon package, then removes the copy. FREESIDE_CORDON_SHARED_LIVE_TEST is
# a FREESIDE_*_LIVE* name, so check.sh keeps HOME and drops its sandbox.
spike_test() { # regex [VAR=value...]
  local re=$1 s; shift
  cp $HERE/cordoned-shared-host-cordon_test.go.txt $TESTFILE
  ( cd $REPO && scrub_env && env FREESIDE_CORDON_SHARED_LIVE_TEST=1 SPIKE_OUT=$ROOT "$@" \
      bash scripts/check.sh daemon test -- ./internal/cordon/ -run "^($re)\$" -v -count=1 -timeout 90m )
  s=$?
  rm -f $TESTFILE
  return $s
}

# Runs $ROOT/scripts/NAME.zsh as one launch's agent (TestSharedScript).
script_launch() { # name variant extra-path
  spike_test TestSharedScript SPIKE_NAME=$1 SPIKE_SCRIPT=$ROOT/scripts/$1.zsh SPIKE_VARIANT=$2 \
    SPIKE_PATH=${3:-} > $ROOT/$1.test.log 2>&1
  log "$1 under $2: test exit $?; end: $(head -1 $ROOT/$1.end 2>/dev/null)"
}

case ${1:-} in
  header) # the report's header facts
    { sw_vers; sysctl hw.model machdep.cpu.brand_string hw.ncpu hw.memsize
      for l in 0 1 2; do sysctl -n hw.perflevel$l.name hw.perflevel$l.physicalcpu | paste -sd' ' -; done
      id; dseditgroup -o checkmember -m $USER admin
      fdesetup isactive; xcodebuild -version | paste -sd' ' -; go version
      rustup --version 2>/dev/null; cargo --version; uv --version; mise --version
      xcrun simctl list runtimes | tail -n +2
      git -C $REPO rev-parse HEAD
      echo "console session: $(swift -e 'import CoreGraphics
let d = CGSessionCopyCurrentDictionary() as? [String: Any] ?? [:]
print(d["kCGSSessionOnConsoleKey"] ?? "nil")' 2>/dev/null)"; } 2>&1 | tee $ROOT/header.txt ;;

  item1-helper) # build v1, sign it, run it as a LaunchAgent, plant the ~/.zshenv marker
    zsh $0 item4-build v1
    mkdir -m 700 -p ~/.spike-helper
    cat > ~/Library/LaunchAgents/com.freeside.spike.helper.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.freeside.spike.helper</string>
<key>ProgramArguments</key><array><string>$ROOT/helper/helper</string><string>serve</string><string>$HOME/.spike-helper/h.sock</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardErrorPath</key><string>$ROOT/helper/serve.log</string>
</dict></plist>
EOF
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.freeside.spike.helper.plist
    sleep 1; $ROOT/helper/helper client ~/.spike-helper/h.sock '{"cmd":"whoami"}'
    # The marker: every zsh that reads ~/.zshenv appends a line, with
    # ssh=yes when sshd started it. cleanup restores the file from the backup.
    [[ -e $ROOT/zshenv.backup || -e $ROOT/zshenv.absent ]] || { [[ -f ~/.zshenv ]] && cp -p ~/.zshenv $ROOT/zshenv.backup || : > $ROOT/zshenv.absent; }
    printf '%s\n' "{ print -r -- \"\$(date -u +%FT%TZ) pid=\$\$ ppid=\$PPID argv0=\$0 ssh=\${SSH_CONNECTION:+yes} cmd=\${ZSH_EXECUTION_STRING:0:40}\" >> $ROOT/zshenv-marker.log; } 2>/dev/null  # freeside spike #3596, removed at cleanup" >> ~/.zshenv ;;

  item1-remote) # print the commands the owner runs on the Studio
    # The mini accepts only the fleet's root-managed key for agent
    # (/etc/ssh/authorized_keys/%u, sshd_config.d/050-fleet.conf).
    cat <<EOF
K=\$(grep -l 'fleet-agent@mance' ~/.ssh/*.pub | head -1); K=\${K%.pub}
M=${2:-agent@HOST}
O=(-i \$K -o IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none)
ssh \$O \$M true; echo "plain ssh exit \$?"
rm -f /tmp/spike-h.sock
ssh \$O -f -N -o ExitOnForwardFailure=yes -L /tmp/spike-h.sock:$HOME/.spike-helper/h.sock \$M; echo "forward exit \$?"
sleep 2; echo '{"cmd":"whoami"}' | nc -U /tmp/spike-h.sock
pkill -f 'spike-h.sock:$HOME'
EOF
    echo "then: grep 'ssh=yes' $ROOT/zshenv-marker.log | tail" ;;

  item4-build) # build the helper with a version string and sign it with the fleet identity
    mkdir -p $ROOT/helper
    ( cd $HERE && go build -ldflags "-X main.version=$2" -o $ROOT/helper/helper-$2 cordoned-shared-host-helper.go )
    ~/.config/fleet/signing/sign ai.freeside.spike.shared-host-helper $ROOT/helper/helper-$2
    codesign -d -r- $ROOT/helper/helper-$2 2>&1 | tail -1
    [[ -e $ROOT/helper/helper ]] || cp $ROOT/helper/helper-$2 $ROOT/helper/helper ;;

  item4-swap) # replace the LaunchAgent's binary in place with VERSION and restart it
    cp $ROOT/helper/helper-$2 $ROOT/helper/helper.new && mv $ROOT/helper/helper.new $ROOT/helper/helper
    launchctl kickstart -k gui/$(id -u)/com.freeside.spike.helper; sleep 2
    $ROOT/helper/helper client ~/.spike-helper/h.sock '{"cmd":"whoami"}' ;;

  item5-build) # the target app, the probe tool, and the actions a run performs
    B=$ROOT/item5; mkdir -p $B/src $B/bin $B/SpikeTarget.app/Contents/MacOS
    cat > $B/src/target.swift <<'EOF_SPIKE'
// SpikeTarget: a window that fills magenta and appends every click and key
// it receives to the log file named by its first argument, so a test reads
// what input reached it instead of trusting a poster's return code.
import AppKit
let logPath = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "@ROOT@/item5/lsopen-target.log"
func log(_ s: String) {
    if let h = FileHandle(forWritingAtPath: logPath) { h.seekToEndOfFile(); h.write((s + "\n").data(using: .utf8)!); h.closeFile() }
    else { FileManager.default.createFile(atPath: logPath, contents: (s + "\n").data(using: .utf8)) }
}
final class V: NSView {
    var typed = ""
    override var acceptsFirstResponder: Bool { true }
    override func draw(_ r: NSRect) {
        NSColor.magenta.setFill(); bounds.fill()
        (typed as NSString).draw(at: NSPoint(x: 20, y: 20), withAttributes: [.font: NSFont.systemFont(ofSize: 28)])
    }
    override func mouseDown(with e: NSEvent) { log("click"); window?.makeFirstResponder(self) }
    override func keyDown(with e: NSEvent) { typed += e.characters ?? ""; log("key \(e.characters ?? "")"); needsDisplay = true }
}
final class D: NSObject, NSApplicationDelegate {
    var w: NSWindow!
    func applicationDidFinishLaunching(_ n: Notification) {
        w = NSWindow(contentRect: NSRect(x: 300, y: 300, width: 480, height: 240), styleMask: [.titled], backing: .buffered, defer: false)
        w.title = "SpikeTarget"; let v = V(frame: w.contentView!.bounds); w.contentView = v
        w.makeKeyAndOrderFront(nil); w.makeFirstResponder(v); NSApp.activate(ignoringOtherApps: true)
        log("launched pid \(getpid())")
    }
}
let app = NSApplication.shared; let d = D(); app.delegate = d; app.setActivationPolicy(.regular); app.run()
EOF_SPIKE
    cat > $B/src/tool.swift <<'EOF_SPIKE'
// spiketool: the spike's probes for item 5.
//   status            AXIsProcessTrusted and CGPreflightScreenCaptureAccess
//   window PID        the first on-screen window of PID: id and bounds
//   click X Y         post a left click at global X Y
//   type TEXT         post TEXT as key events
//   pixel PNG         the center pixel of PNG as r g b
import AppKit
import ApplicationServices
let a = CommandLine.arguments
func post(_ e: CGEvent?) { e?.post(tap: .cghidEventTap); usleep(20000) }
switch a.count > 1 ? a[1] : "" {
case "status":
    print("ax-trusted \(AXIsProcessTrusted()) screen-capture-preflight \(CGPreflightScreenCaptureAccess())")
case "prompt":
    let opts = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
    print("ax-trusted-with-prompt \(AXIsProcessTrustedWithOptions(opts))")
case "window":
    let pid = Int(a[2])!
    let ws = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
    for w in ws where w[kCGWindowOwnerPID as String] as? Int == pid {
        let b = w[kCGWindowBounds as String] as? [String: Double] ?? [:]
        print(w[kCGWindowNumber as String] as? Int ?? 0, Int(b["X"] ?? 0), Int(b["Y"] ?? 0), Int(b["Width"] ?? 0), Int(b["Height"] ?? 0),
              "name:\(w[kCGWindowName as String] as? String ?? "<none>")")
        exit(0)
    }
    exit(1)
case "click":
    let p = CGPoint(x: Double(a[2])!, y: Double(a[3])!)
    post(CGEvent(mouseEventSource: nil, mouseType: .leftMouseDown, mouseCursorPosition: p, mouseButton: .left))
    post(CGEvent(mouseEventSource: nil, mouseType: .leftMouseUp, mouseCursorPosition: p, mouseButton: .left))
case "type":
    for ch in a[2].utf16 {
        var c = ch
        for down in [true, false] {
            let e = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: down)
            e?.keyboardSetUnicodeString(stringLength: 1, unicodeString: &c); post(e)
        }
    }
case "pixel":
    guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: a[2]) as CFURL, nil),
          let img = CGImageSourceCreateImageAtIndex(src, 0, nil) else { print("unreadable"); exit(1) }
    let rep = NSBitmapImageRep(cgImage: img)
    let c = rep.colorAt(x: img.width / 2, y: img.height / 2)?.usingColorSpace(.deviceRGB)
    print(img.width, img.height, Int((c?.redComponent ?? 0) * 255), Int((c?.greenComponent ?? 0) * 255), Int((c?.blueComponent ?? 0) * 255))
default:
    exit(2)
}
EOF_SPIKE
    cat > $B/act.zsh <<'EOF_SPIKE'
# Item 5's actions, run under the cordon through the helper chain:
# start the target (open: Launch Services; direct: exec), capture its
# window, click it, type into it, and print what reached it.
set -u
B=@ROOT@/item5; T=$B/bin/spiketool; out=$1; mkdir -p $out; log=$out/target.log; : > $log
echo "status $($T status)"
if [[ $2 == open ]]; then log=$B/lsopen-target.log; : > $log; /usr/bin/open -n -a $B/SpikeTarget.app --args $log; echo "open exit $?"
else $B/SpikeTarget.app/Contents/MacOS/SpikeTarget $log >/dev/null 2>&1 &!; echo "direct start"; fi
sleep 3
pid=$(pgrep -n -x SpikeTarget); echo "pid ${pid:-none}"
w=($($T window ${pid:-0})); echo "window ${w[*]:-none}"
if (( ${#w} >= 5 )); then
  /usr/sbin/screencapture -x -o -l ${w[1]} $out/cap1.png; echo "capture exit $?"
  echo "cap1 center (w h r g b): $($T pixel $out/cap1.png 2>&1)"
  $T click $(( w[2] + w[4] / 2 )) $(( w[3] + w[5] / 2 )); echo "click exit $?"
  $T type spike3596; echo "type exit $?"
  sleep 1
fi
echo "target log: $(tr '\n' '|' < $log)"
EOF_SPIKE
    sed -i '' "s|@ROOT@|$ROOT|g" $B/src/target.swift $B/act.zsh
    swiftc -O -o $B/bin/spiketool $B/src/tool.swift
    swiftc -O -o $B/SpikeTarget.app/Contents/MacOS/SpikeTarget $B/src/target.swift
    cat > $B/SpikeTarget.app/Contents/Info.plist <<'EOF_SPIKE'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>SpikeTarget</string>
<key>CFBundleIdentifier</key><string>ai.freeside.spike.target</string>
<key>CFBundleName</key><string>SpikeTarget</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
EOF_SPIKE
    ;;

  item5) # one stage: the actions through the LaunchAgent chain under each rule file
    # LABEL names the permission state the owner set (s1-none, s2-sr,
    # s3-sr-ax, s4-ax, s5-v2, s6-restart). Keep hands off the console
    # while it runs: the target window takes the keyboard.
    B=$ROOT/item5
    for spec in nolsopen:open default:direct default:open; do v=${spec%%:*} m=${spec##*:}
      print -r -- "== $2 $v $m"
      $ROOT/helper/helper client ~/.spike-helper/h.sock "{\"cmd\":\"run\",\"rules\":\"$ROOT/rules-$v.sb\",\"params\":\"$ROOT/params-$v.txt\",\"dir\":\"$ROOT/helper-run-$v\",\"argv\":[\"/bin/zsh\",\"-f\",\"$B/act.zsh\",\"$B/$2-$v-$m\",\"$m\"]}" \
        | python3 -I -c 'import json,sys; r=json.load(sys.stdin); print("chain exit", r["exit"], r.get("error","")); print(r.get("output", ""))'
      pkill -x SpikeTarget; sleep 1
    done 2>&1 | tee -a $B/stages.log ;;

  item5-prompt) # ask for the Accessibility prompt from inside the chain
    $ROOT/helper/helper client ~/.spike-helper/h.sock "{\"cmd\":\"run\",\"rules\":\"$ROOT/rules-default.sb\",\"params\":\"$ROOT/params-default.txt\",\"dir\":\"$ROOT/helper-run-default\",\"argv\":[\"$ROOT/item5/bin/spiketool\",\"prompt\"]}"
    open "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_Accessibility" ;;

  claude-dirs) # can a run read another Claude config directory on the account?
    $ROOT/helper/helper client ~/.spike-helper/h.sock "{\"cmd\":\"run\",\"rules\":\"$ROOT/rules-default.sb\",\"params\":\"$ROOT/params-default.txt\",\"dir\":\"$ROOT/helper-run-default\",\"argv\":[\"/bin/zsh\",\"-fc\",\"for f in ~/.claude ~/.claude-*(N/); do if ls \$f >/dev/null 2>&1; then echo listable \$f; else echo denied \$f; fi; done\"]}" \
      | python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("output", ""))' ;;

  item2) # the live suite as written, then under the account's real configuration
    ( cd $REPO && scrub_env && FREESIDE_CORDON_LIVE_TEST=1 bash scripts/check.sh daemon test -- \
        ./internal/cordon/... -run Live -v -count=1 ) > $ROOT/item2-live-${2:-console}.log 2>&1
    log "item2 live suite (${2:-console}): exit $?"
    spike_test 'TestSharedRealConfig|TestSharedRender' > $ROOT/item2-realconfig-${2:-console}.log 2>&1
    log "item2 real configuration (${2:-console}): exit $?"
    grep -h -E '^(--- |\s+--- )|skipped|SKIP' $ROOT/item2-*-${2:-console}.log
    # Which of the rule file's secret paths exist on this account: a path
    # that doesn't exist passes the suite's read probe without proving it.
    for p in .ssh .config/gh .git-credentials .netrc .aws .config/gcloud .azure .kube \
        .docker/config.json .codex .claude .claude.json .freeside Library/Keychains \
        "Library/Application Support/Google/Chrome" "Library/Application Support/Firefox" \
        Library/Safari Library/Cookies "Library/Application Support/com.apple.container" \
        "Library/Application Support/Freeside" "Library/Application Support/Freeside Dev"; do
      [[ -e $HOME/$p ]] && echo "exists  ~/$p" || echo "absent  ~/$p"
    done | tee $ROOT/item2-secret-paths.txt ;;

  item3) # N ends through the backend and by hand, under the label's condition
    echo "${3:-idle} before: $(uptime)" | tee -a $ROOT/item3-load-check.txt
    spike_test TestSharedEnds SPIKE_N=${2:-50} SPIKE_LABEL=${3:-idle} > $ROOT/item3-${3:-idle}.log 2>&1
    echo "${3:-idle} after: $(uptime)" | tee -a $ROOT/item3-load-check.txt
    log "item3 ${3:-idle}: exit $?"
    zsh $0 summarize3 ${3:-idle} ;;

  summarize3)
    python3 -I - $ROOT/item3-ends-$2.jsonl <<'EOF' | tee $ROOT/item3-summary-$2.txt
import json, sys, statistics as st
rows = [json.loads(l) for l in open(sys.argv[1])]
for path in ("backend", "manual"):
    r = [x for x in rows if x["path"] == path]
    t = sorted(x["end_ms"] for x in r)
    print(f"{path}: n={len(r)} proved={sum(x['proved'] for x in r)} "
          f"survivors={sum(max(0, x['survivors_alive_after']) for x in r)} "
          f"pids-files-missing={sum(1 for x in r if x['survivors_alive_after'] < 0)} "
          f"ms min/median/p95/max={t[0]:.1f}/{st.median(t):.1f}/{t[int(0.95*(len(t)-1))]:.1f}/{t[-1]:.1f}")
    for x in r:
        if not x["proved"]:
            print("  unproven:", x["n"], x.get("error", "")[:300])
    if path == "manual":
        p = [q for x in r for q in x["passes"]]
        print(f"  passes per end: {sorted({len(x['passes']) for x in r})}; "
              f"processes listed per pass: {min(q['listed'] for q in p)}-{max(q['listed'] for q in p)}; "
              f"unsettled seen: {sum(q['unsettled'] for q in p)}; replaced seen: {sum(q['replaced'] for q in p)}; "
              f"killed per end: {sorted({sum(q['killed'] for q in x['passes']) for x in r})}")
EOF
    cat $ROOT/item3-foreign-$2.json ;;

  load-start) # unconfined, fork-heavy work on the account beside the ends
    mkdir -p $ROOT/load
    [[ -d $ROOT/load/spikecrate ]] || { cargo new -q --vcs none $ROOT/load/spikecrate &&
      printf 'regex = "1"\nserde_json = "1"\n' >> $ROOT/load/spikecrate/Cargo.toml; }
    ( while :; do (cd $REPO/daemon && go test -count=1 ./... >/dev/null 2>&1); done ) &
    echo $! > $ROOT/load/go.pid
    ( while :; do (cd $ROOT/load/spikecrate && cargo clean -q && cargo build -q >/dev/null 2>&1); done ) &
    echo $! > $ROOT/load/cargo.pid
    ( while :; do xcodebuild -project $REPO/app/Freeside.xcodeproj -scheme FreesideMac \
        -destination platform=macOS -derivedDataPath $ROOT/load/dd -skipPackagePluginValidation \
        -skipMacroValidation CODE_SIGNING_ALLOWED=NO clean build >/dev/null 2>&1; done ) &
    echo $! > $ROOT/load/xcode.pid
    log "load started: go test ./..., cargo build, xcodebuild FreesideMac, each in a loop" ;;

  load-stop)
    for f in $ROOT/load/*.pid; do kill $(cat $f) 2>/dev/null; done
    for f in $ROOT/load/*.pid; do pkill -P $(cat $f) 2>/dev/null; done
    sleep 1
    pkill -f "derivedDataPath $ROOT/load/dd" 2>/dev/null
    pkill -f "$ROOT/load/spikecrate" 2>/dev/null; pkill -f 'go test -count=1 ./...' 2>/dev/null
    pgrep -fl "go test|cargo build|xcodebuild" | head -3
    log "load stopped" ;;

  item8) # toolchains from the version managers, inside one launch
    # What each manager already holds, so a warm cache is recorded as warm.
    { echo "-- before: what the managers already hold"
      for d in ~/go/pkg/mod/golang.org/toolchain* ~/Library/Caches/go-build ~/.cache/uv \
          ~/.local/share/uv/python ~/.cargo/registry ~/.rustup/toolchains ~/.local/share/mise \
          ~/Library/Caches/mise ~/.local/state/mise ~/.config/mise; do
        [[ -e $d ]] && echo "present $d ($(du -sh $d 2>/dev/null | cut -f1))" || echo "absent  $d"
      done; } | tee $ROOT/item8-before.txt
    ls ~/.local/state/mise/trusted-configs 2>/dev/null | wc -l | xargs echo "mise trusted-configs entries before:" \
      | tee -a $ROOT/item8-before.txt
    cat > $ROOT/scripts/item8.zsh <<'EOF'
set -u
step() { local l=$1 t0=$SECONDS s; shift; print -r -- "== $l: $*"; "$@" < /dev/null 2>&1 | tail -15; s=${pipestatus[1]}
  print -r -- "== $l: exit $s in $((SECONDS - t0)) s"; }
print -r -- "PATH=$PATH"
# Go: a module whose GOTOOLCHAIN names a release that isn't installed.
mkdir -p go && cd go
print 'module example.com/spike\n\ngo 1.26\n' > go.mod
print 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("built") }\n' > main.go
step go-build env GOTOOLCHAIN=go1.26.5 go build -o built .
step go-version env GOTOOLCHAIN=go1.26.5 go version
cd ..
# uv: a project that requires Python 3.12.
mkdir -p py && cd py
print '[project]\nname = "spike"\nversion = "0.1.0"\nrequires-python = ">=3.12,<3.13"\ndependencies = ["six"]\n' > pyproject.toml
step uv-sync uv sync --python 3.12
step uv-run uv run python -c 'import sys, six; print(sys.version.split()[0], six.__version__)'
cd ..
# cargo: a crate with one dependency.
mkdir -p rs/src && cd rs
print '[package]\nname = "spike"\nversion = "0.1.0"\nedition = "2021"\n\n[dependencies]\nitoa = "1"\n' > Cargo.toml
print 'fn main() { println!("{}", itoa::Buffer::new().format(42)); }' > src/main.rs
step cargo-build cargo build
step cargo-run cargo run -q
cd ..
# mise: trust given only through the environment.
mkdir -p mise && cd mise
print '[tools]\njq = "1.7.1"\n' > mise.toml
step mise-untrusted mise install
step mise-install env MISE_TRUSTED_CONFIG_PATHS=$PWD mise install
step mise-exec env MISE_TRUSTED_CONFIG_PATHS=$PWD mise exec -- jq --version
cd ..
EOF
    touch $ROOT/item8.stamp; sleep 1
    script_launch item8 default \
      /opt/homebrew/bin:/opt/homebrew/opt/rustup/bin:$HOME/.cargo/bin:$HOME/.local/bin:$HOME/.local/share/mise/shims
    # Everything written outside the worktree since the stamp, by directory.
    # Other sessions on the account write too; the report names which
    # directories are the managers'.
    { echo "-- after: files changed since the stamp, by directory (3 levels), count"
      for base in $HOME $(getconf DARWIN_USER_TEMP_DIR) $(getconf DARWIN_USER_CACHE_DIR); do
        find ${base%/} -newer $ROOT/item8.stamp -type f 2>/dev/null \
          | awk -v b=${base%/} '{ sub("^" b "/", ""); n = split($0, a, "/"); p = a[1];
              for (i = 2; i <= 3 && i < n; i++) p = p "/" a[i]; print b " " p }' | sort | uniq -c | sort -rn | head -40
      done
      ls ~/.local/state/mise/trusted-configs 2>/dev/null | wc -l | xargs echo "mise trusted-configs entries after:"
      find ~/.local/state/mise -newer $ROOT/item8.stamp 2>/dev/null | sed "s|$HOME|~|"; } | tee $ROOT/item8-after.txt ;;

  item6) # the Simulator, under the rule file without the CoreSimulator denial
    sim_ps() { ps -ax -o pid=,ppid=,user=,comm= | grep -E 'launchd_sim|CoreSimulator|simdiskimaged|SimulatorTrampoline|FreesideIOS|Freeside\.app' \
      | grep -v grep | awk '{n=split($4,a,"/"); print ($2==1 ? "parent=launchd" : "parent=other"), $3, a[n]}' | sort | uniq -c; }
    { echo "-- before"; sim_ps; } | tee $ROOT/item6-ps.txt
    cat > $ROOT/scripts/item6.zsh <<EOF
set -u
step() { local l=\$1 t0=\$SECONDS s; shift; print -r -- "== \$l"; "\$@" < /dev/null > \$PWD/\$l.log 2>&1; s=\$?
  tail -3 \$PWD/\$l.log; print -r -- "== \$l: exit \$s in \$((SECONDS - t0)) s"; }
cp -R $REPO/app ./app
udid=\$(xcrun simctl create freeside-spike-3596 com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro com.apple.CoreSimulator.SimRuntime.iOS-27-2 </dev/null)
print -r -- "created \$udid"; print -r -- \$udid > $ROOT/sim-udid
step boot xcrun simctl boot \$udid
step bootstatus xcrun simctl bootstatus \$udid
step build xcodebuild -project app/Freeside.xcodeproj -scheme FreesideIOS -destination id=\$udid \
  -derivedDataPath \$PWD/dd -clonedSourcePackagesDirPath \$PWD/sp -skipPackagePluginValidation -skipMacroValidation \
  -IDEPackageSupportDisableManifestSandbox=YES -IDEPackageSupportDisablePluginExecutionSandbox=YES \
  'OTHER_SWIFT_FLAGS=\$(inherited) -disable-sandbox' CODE_SIGNING_ALLOWED=NO build
app=\$(ls -d \$PWD/dd/Build/Products/Debug-iphonesimulator/*.app | head -1)
bid=\$(/usr/libexec/PlistBuddy -c 'Print CFBundleIdentifier' \$app/Info.plist)
step install xcrun simctl install \$udid \$app
step launch xcrun simctl launch \$udid \$bid -FreesideMock YES
sleep 5
step screenshot xcrun simctl io \$udid screenshot \$PWD/sim.png
sips -g pixelWidth -g pixelHeight \$PWD/sim.png | tail -2
cp \$PWD/sim.png $ROOT/item6-sim.png 2>/dev/null
EOF
    script_launch item6 nosim
    # The run is over and its proof passed or failed; what is still alive?
    { echo "-- after the run's end, no simctl shutdown"; sim_ps
      echo "-- device state: $(xcrun simctl list devices | grep freeside-spike-3596)"
      xcrun simctl shutdown $(cat $ROOT/sim-udid) 2>&1; sleep 5
      echo "-- after simctl shutdown"; sim_ps
      xcrun simctl delete $(cat $ROOT/sim-udid) 2>&1; sleep 5
      echo "-- after simctl delete"; sim_ps; } | tee -a $ROOT/item6-ps.txt ;;

  item7) # headless Chrome under the default rule file
    CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    "$CHROME" --version | tee $ROOT/item7-version.txt
    cat > $ROOT/scripts/item7.zsh <<EOF
set -u
print '<html><body style="background:#0a0"><h1>spike 3596</h1></body></html>' > page.html
python3 -m http.server 8765 --bind 127.0.0.1 >/dev/null 2>&1 &!
sleep 1
try() { local l=\$1 s; shift
  /usr/bin/perl -e 'alarm shift; exec @ARGV' 90 "$CHROME" --headless=new --user-data-dir=\$PWD/profile-\$l \
    --screenshot=\$PWD/shot-\$l.png --window-size=800,600 "\$@" http://127.0.0.1:8765/page.html < /dev/null > \$PWD/\$l.log 2>&1
  s=\$?; print -r -- "== \$l (\$*): exit \$s, screenshot \$([[ -s \$PWD/shot-\$l.png ]] && echo yes || echo no)"
  grep -i -E 'sandbox|keychain|denied|not permitted|fatal|error' \$PWD/\$l.log | cut -c1-200 | sort | uniq -c | sort -rn | head -6; }
try plain
try nosandbox --no-sandbox
try mockkeychain --use-mock-keychain
try both --no-sandbox --use-mock-keychain
cp \$PWD/shot-*.png $ROOT/ 2>/dev/null
pkill -f 'http.server 8765'
EOF
    touch $ROOT/item7.stamp; sleep 1
    script_launch item7 default
    { echo "-- files Chrome changed outside the run since the stamp"
      find $HOME "$(getconf DARWIN_USER_CACHE_DIR)" "$(getconf DARWIN_USER_TEMP_DIR)" -newer $ROOT/item7.stamp -type f 2>/dev/null \
        | grep -i -E 'google|chrom' | sed "s|$HOME|~|" | awk -F/ '{print $1"/"$2"/"$3"/"$4"/"$5}' | sort | uniq -c; } | tee $ROOT/item7-outside.txt ;;

  mise-variants) # which rule breaks mise's TLS: drop one keychain denial at a time
    python3 -I - $ROOT <<'EOF'
import sys
root = sys.argv[1]
s = open(f"{root}/rules-default.sb").read()
lookup = '(deny mach-lookup\n  (global-name "com.apple.SecurityServer")\n  (global-name "com.apple.securityd.xpc"))\n'
files = '(deny file-read* file-write*\n  (subpath (string-append (param "HOME") "/Library/Keychains")))\n'
assert lookup in s and files in s
open(f"{root}/rules-nosecd.sb", "w").write(s.replace(lookup, ""))
open(f"{root}/rules-nokcfile.sb", "w").write(s.replace(files, ""))
open(f"{root}/rules-nokc.sb", "w").write(s.replace(lookup, "").replace(files, ""))
EOF
    params=("${(@f)$(cat $ROOT/params-default.txt)}")
    # Each variant gets empty mise data and cache directories, so each one
    # downloads, and nothing lands in the account's own mise directories.
    for v in default nosecd nokcfile nokc certfile; do d=$ROOT/mise-$v-$$
      mkdir -p $d; printf '[tools]\njq = "1.7.1"\n' > $d/mise.toml
      extra=(); rf=$ROOT/rules-$v.sb; [[ $v == certfile ]] && { extra=(SSL_CERT_FILE=/etc/ssl/cert.pem); rf=$ROOT/rules-default.sb; }
      o=$(cd $d && env -i HOME=$HOME PATH=/opt/homebrew/bin:/usr/bin:/bin TMPDIR=$ROOT/helper-run-default MISE_TRUSTED_CONFIG_PATHS=$d MISE_DATA_DIR=$d/data MISE_CACHE_DIR=$d/cache $extra \
        /usr/bin/sandbox-exec -f $rf $params /opt/homebrew/bin/mise install </dev/null 2>&1 | grep -m1 -o -E 'installed 1 tool|bad certificate format|already installed')
      echo "$v: $o"
    done | tee $ROOT/mise-variants.txt ;;

  mise-trust) # unconfined: does a plain install trust the config, and does env trust write state?
    for mode in plain env; do d=$ROOT/mise-trust-$mode; mkdir -p $d; printf '[tools]\njq = "1.7.1"\n' > $d/mise.toml
      before=$(ls ~/.local/state/mise/trusted-configs 2>/dev/null | wc -l)
      if [[ $mode == env ]]; then (cd $d && MISE_TRUSTED_CONFIG_PATHS=$d mise install </dev/null >/dev/null 2>&1)
      else (cd $d && mise install </dev/null >/dev/null 2>&1); fi
      echo "$mode: trusted-configs $before -> $(ls ~/.local/state/mise/trusted-configs | wc -l)"
    done | tee $ROOT/mise-trust.txt ;;

  item1-loopback) # the loopback attempt: sshd reads no key from ~/.ssh on this host
    # ~/.ssh didn't exist; cleanup removes the key file. A loopback-only key.
    mkdir -p $ROOT/ssh; [[ -f $ROOT/ssh/id ]] || ssh-keygen -q -t ed25519 -N '' -C spike-3596-loopback -f $ROOT/ssh/id
    mkdir -m 700 -p ~/.ssh; printf 'from="127.0.0.1,::1" %s\n' "$(cat $ROOT/ssh/id.pub)" > ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys
    ssh -F /dev/null -i $ROOT/ssh/id -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
      -o UserKnownHostsFile=$ROOT/ssh/known_hosts agent@127.0.0.1 true; echo "loopback ssh exit $?"
    rm -f ~/.ssh/authorized_keys ;;

  item9) # memory pressure level, unconfined and inside a launch
    echo "unconfined: $(sysctl -n kern.memorystatus_vm_pressure_level)" | tee -a $ROOT/item9.txt
    print 'for i in 1 2 3; do print -r -- "in launch: $(sysctl -n kern.memorystatus_vm_pressure_level)"; sleep 1; done' \
      > $ROOT/scripts/item9-${2:-idle}.zsh
    script_launch item9-${2:-idle} default
    sed "s/^/${2:-idle} /" $ROOT/item9-${2:-idle}.transcript | tee -a $ROOT/item9.txt ;;

  item9-warn) # allocate until the level reaches 2 (warning), read it, release
    # Incompressible 256 MiB chunks, so the compressor can't absorb them;
    # the allocator stops at level 2 or 40 GiB and holds until released.
    python3 -I - $ROOT/item9-alloc.flag <<'EOF' > $ROOT/item9-alloc.log 2>&1 &
import os, subprocess, sys, time
flag, held = sys.argv[1], []
lvl = lambda: int(subprocess.run(["sysctl", "-n", "kern.memorystatus_vm_pressure_level"], capture_output=True, text=True).stdout)
while lvl() < 2 and len(held) < 160:
    held.append(os.urandom(256 << 20)); print(len(held) * 256, "MiB level", lvl(), flush=True)
print("holding at level", lvl(), "with", len(held) * 256, "MiB", flush=True)
open(flag, "w").close()
while os.path.exists(flag): time.sleep(0.5)
EOF
    apid=$!
    while [[ ! -e $ROOT/item9-alloc.flag ]] && kill -0 $apid 2>/dev/null; do sleep 1; done
    tail -1 $ROOT/item9-alloc.log | tee -a $ROOT/item9.txt
    zsh $0 item9 warn
    $ROOT/helper/helper client ~/.spike-helper/h.sock \
      "{\"cmd\":\"run\",\"rules\":\"$ROOT/rules-default.sb\",\"params\":\"$ROOT/params-default.txt\",\"dir\":\"$ROOT/helper-run-default\",\"argv\":[\"/usr/sbin/sysctl\",\"-n\",\"kern.memorystatus_vm_pressure_level\"]}" \
      | python3 -I -c 'import json,sys; r=json.load(sys.stdin); print("warn LaunchAgent chain:", r.get("output", "").strip(), "exit", r["exit"])' | tee -a $ROOT/item9.txt
    rm -f $ROOT/item9-alloc.flag; wait $apid
    sleep 5; echo "after release: $(sysctl -n kern.memorystatus_vm_pressure_level)" | tee -a $ROOT/item9.txt ;;

  cleanup)
    zsh $0 load-stop 2>/dev/null
    rm -f $TESTFILE
    launchctl bootout gui/$(id -u)/com.freeside.spike.helper 2>/dev/null
    rm -f ~/Library/LaunchAgents/com.freeside.spike.helper.plist
    [[ -f $ROOT/zshenv.backup ]] && { cp -p $ROOT/zshenv.backup ~/.zshenv; cmp ~/.zshenv $ROOT/zshenv.backup && log "~/.zshenv restored"; }
    [[ -f $ROOT/zshenv.absent ]] && rm -f ~/.zshenv
    [[ -f $ROOT/sim-udid ]] && xcrun simctl delete $(cat $ROOT/sim-udid) 2>/dev/null
    rm -f ~/.spike-helper/h.sock; rmdir ~/.spike-helper 2>/dev/null
    # mise's trust and tracking links that name the spike's directories
    # (item 8), and the jq it installed into a share directory that didn't
    # exist before the spike.
    for l in ~/.local/state/mise/{trusted,tracked}-configs/*(N@); do
      [[ $(readlink $l) == (/private/tmp/cdn*|$ROOT*) ]] && rm -f $l
    done
    rmdir ~/.local/state/mise/incomplete-installs/jq 2>/dev/null
    echo "left for the owner: ~/.local/share/mise (jq installs), ~/.ssh/agent (launchd ssh-agent's socket)"
    echo "Screen Recording and Device Control entries for 'helper' stay until the owner removes them"
    git -C $REPO status --short daemon/ ;;

  *) sed -n '7,12p' $0; exit 2 ;;
esac
