#!/bin/zsh
# Cross-process probes from the cordoned-verifier spike (#3526), kept apart
# from its harness so #3470's conformance suite can aim them at the daemon.
# Run each one under the rule file being tested; each prints one line,
# "<probe> <result>", and never a value it read.
#
# Usage: zsh cordoned-verifier-probe.zsh PROBE TARGET [args]
#   signal PID LSTART            SIGSTOP then SIGCONT to PID: can this
#                                process stop (and so kill) the target?
#   attach PID LSTART            task_for_pid, then lldb attach and detach
#   env PID LSTART MARKER...     can this process read PID's environment
#                                (sysctl KERN_PROCARGS2, as ps eww does)
#                                and does it hold MARKER (given in pieces,
#                                so this probe's own argv never matches)?
#   kickstart LABEL              launchctl kickstart -k gui/<uid>/LABEL
#   lkill LABEL                  launchctl kill TERM gui/<uid>/LABEL
#   stop LABEL                   launchctl stop LABEL (launchd sends the signal)
#   bootout LABEL                launchctl bootout gui/<uid>/LABEL; the caller
#                                bootstraps the service again
#
# A pid probe acts only when the target's start time, as `ps -o lstart=`
# prints it, equals LSTART, so a pid reused by another process is never
# hit. The caller reads LSTART when it starts the target, outside any rule
# file (ps is setuid and a rule file can't exec it); this probe compares it
# with the start time sysctl(KERN_PROC_PID) gives. A label probe acts only on
# a label under com.freeside.spike., the prefix the spike's own services use.
set -u
# zsh writes each here-document to a file under TMPPREFIX, /tmp by default,
# which a verifier rule file refuses; the caller's TMPDIR is writable.
TMPPREFIX=${TMPDIR:-/tmp}/zsh
probe=${1:?probe}; target=${2:?target}; shift 2

# Prints the start time of pid as ps -o lstart= does, via sysctl, which a
# rule file lets through (ps itself can't run under one).
start_time() {
  /usr/bin/python3 -I - $1 <<'EOF'
import ctypes, ctypes.util, sys, time
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
buf = ctypes.create_string_buffer(1024)
size = ctypes.c_size_t(1024)
mib = (ctypes.c_int * 4)(1, 14, 1, int(sys.argv[1]))  # CTL_KERN KERN_PROC KERN_PROC_PID
if libc.sysctl(mib, 4, buf, ctypes.byref(size), None, 0) != 0 or size.value == 0:
    sys.exit(1)
sec = ctypes.c_long.from_buffer(buf, 0).value  # kp_proc.p_starttime.tv_sec
print(time.strftime("%a %b %e %H:%M:%S %Y", time.localtime(sec)))
EOF
}
guard_pid() { # pid lstart: exits 3 unless the pid is the process started then
  local cur=$(start_time $1 2>/dev/null)
  [[ ${(j: :)${=cur}} == ${(j: :)${=2}} ]] || { echo "$probe target-mismatch pid=$1"; exit 3; }
}
guard_label() {
  [[ $1 == com.freeside.spike.* ]] || { echo "$probe refused label=$1"; exit 3; }
}

case $probe in
  signal)
    guard_pid $target ${1:?LSTART}
    kill -s STOP $target 2>/dev/null; s=$?
    kill -s CONT $target 2>/dev/null
    echo "signal stop-exit=$s" ;;

  attach)
    guard_pid $target ${1:?LSTART}
    kr=$(/usr/bin/python3 -I - $target <<'EOF'
import ctypes, ctypes.util, sys
libc = ctypes.CDLL(ctypes.util.find_library("c"))
port = ctypes.c_uint(0)
self_task = ctypes.c_uint.in_dll(libc, "mach_task_self_")
print(libc.task_for_pid(self_task, int(sys.argv[1]), ctypes.byref(port)))
EOF
)
    # lldb asks for developer-tools authorization when the host hasn't
    # granted it; the alarm ends a run that waits on that prompt (exit 142).
    out=$(perl -e 'alarm 30; exec @ARGV or exit 127' /usr/bin/lldb --batch -p $target \
      -o 'process detach' 2>&1); s=$?
    a=no; [[ $out == *"Process $target stopped"* || $out == *"Process $target detached"* ]] && a=yes
    echo "attach task_for_pid-kr=$kr lldb-exit=$s lldb-attached=$a" ;;

  env)
    guard_pid $target ${1:?LSTART}; shift
    /usr/bin/python3 -I - $target "$@" <<'EOF'
import ctypes, ctypes.util, sys
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
pid, marker = int(sys.argv[1]), "".join(sys.argv[2:]).encode()
buf = ctypes.create_string_buffer(1 << 20)
size = ctypes.c_size_t(1 << 20)
mib = (ctypes.c_int * 3)(1, 49, pid)  # CTL_KERN, KERN_PROCARGS2
ok = libc.sysctl(mib, 3, buf, ctypes.byref(size), None, 0) == 0
err = ctypes.get_errno()
hit = ok and marker in buf.raw[:size.value]
print(f"env readable={'yes' if ok else 'no'} errno={0 if ok else err} marker={'yes' if hit else 'no'}")
EOF
    ;;

  kickstart|lkill|stop|bootout)
    guard_label $target
    svc=gui/$(id -u)/$target
    before=$(launchctl print $svc 2>/dev/null | awk '/^\tpid = / {print $3}')
    case $probe in
      kickstart) launchctl kickstart -k $svc ;;
      lkill) launchctl kill TERM $svc ;;
      stop) launchctl stop $target ;;
      bootout) launchctl bootout $svc ;;
    esac >/dev/null 2>&1; s=$?
    sleep 2
    after=$(launchctl print $svc 2>/dev/null | awk '/^\tpid = / {print $3}')
    echo "$probe exit=$s pid-changed=$([[ -n $before && $before != "$after" ]] && echo yes || echo no)" ;;

  *) sed -n '2,27p' $0; exit 2 ;;
esac
