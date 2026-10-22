#!/bin/zsh
# Ward density benchmark on Apple container. Method: docs/spikes/ward-density.md.
# Usage: zsh ward-density-bench.zsh idle | coldstart | build | concurrent N | cleanup
# Env: IMG (project image), HELPER_IMG (exporter image), SEED (host dir holding a git archive of the repository), OUT (log dir)
set -u
: ${IMG:?} ${SEED:?} ${OUT:?}
HELPER_IMG=${HELPER_IMG:-}
PREFIX=freeside-bench
WRITER_CPUS=4 WRITER_MEM=2048M          # sizes.go writer default; not changed by this benchmark
SAMPLE_EVERY=10
MAIN='trap "exit 0" TERM; while :; do sleep 3600 & wait; done'   # exits promptly on SIGTERM, like a well-behaved agent
mkdir -p "$OUT"; touch "$OUT/vzpids"
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/events.log"; }
now() { date +%s.%N; }
dt() { printf "%.2f" $(echo "$2 - $1" | bc -l); }

mib() { # "237 MB" | "6705 KB" | "1.2 GB" -> MiB
  awk '{v=$1; u=$2; if (u=="KB") v=v/1024; else if (u=="GB") v=v*1024; else if (u=="B") v=v/1048576; printf "%.0f", v}'
}
vz_lookup() { # name -> Virtualization XPC pid that holds this container's files; recorded in vzpids
  local n=$1 v
  for v in $(pgrep -f "com.apple.Virtualization.VirtualMachine"); do
    if lsof -p $v 2>/dev/null | grep -q "containers/$n/"; then echo "$n $v" >> "$OUT/vzpids"; echo $v; return; fi
  done
}
vz_pid() { awk -v n=$1 '$1==n {p=$2} END {print p}' "$OUT/vzpids"; }

host_row() { # prints pressure free inactive compressor swap idle diskfree foreign
  local vm=$(vm_stat)
  local pressure=$(sysctl -n kern.memorystatus_vm_pressure_level)
  local free=$(echo "$vm" | awk '/Pages free/ {printf "%.0f", $3*16384/1048576}')
  local inact=$(echo "$vm" | awk '/Pages inactive/ {printf "%.0f", $3*16384/1048576}')
  local comp=$(echo "$vm" | awk '/occupied by compressor/ {printf "%.0f", $5*16384/1048576}')
  local swap=$(sysctl -n vm.swapusage | awk '{gsub("M","",$6); printf "%.0f", $6}')
  local idle=$(top -l 2 -n 0 -s 1 | awk '/CPU usage/ {v=$7} END {gsub("%","",v); print v}')
  local disk=$(df -g "$HOME" | awk 'NR==2 {print $4}')
  local foreign=$(container list --format json 2>/dev/null | jq "[.[] | .configuration | select((.id|startswith(\"$PREFIX\")|not) and .id != \"buildkit\" and (.image.reference|contains(\"library/registry\")|not))] | length")
  echo "$pressure $free $inact $comp $swap $idle $disk $foreign"
}
# samples.tsv: ts label kind name vzpid footprint_mib peak_mib pressure free_mib inactive_mib compressor_mib swap_mib cpu_idle disk_free_gib foreign_containers (running, excluding benchmark, buildkit and registry containers)
sample() {
  local label=$1; shift
  local ts=$(date +%s) h=($(host_row)) n p fp pk
  HOST=($h)
  printf "%s\t%s\thost\t-\t-\t-\t-\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n" $ts $label $h >> "$OUT/samples.tsv"
  for n in "$@"; do
    p=$(vz_pid $n); fp=-; pk=-
    if [[ -n "$p" ]] && kill -0 $p 2>/dev/null; then
      local f=$(footprint $p 2>/dev/null)
      fp=$(echo "$f" | awk '/phys_footprint:/ {print $2, $3}' | mib)
      pk=$(echo "$f" | awk '/phys_footprint_peak:/ {print $2, $3}' | mib)
    fi
    printf "%s\t%s\tward\t%s\t%s\t%s\t%s\n" $ts $label $n ${p:--} $fp $pk >> "$OUT/samples.tsv"
  done
}
guard_ok() { # pressure 4 is critical; keep 20 GiB of disk for swap growth
  (( HOST[1] < 4 && HOST[7] >= 20 ))
}

seed_volume() {
  local n=$1 t0=$(now)
  container volume delete $n-ws >/dev/null 2>&1
  container volume create -s 4096M --label freeside.bench=density $n-ws >/dev/null
  container run --rm --name $n-seed --cpus 2 --memory 512M --network none \
    --mount type=bind,source="$SEED",target=/seed,readonly \
    --mount type=volume,source=$n-ws,target=/workspace \
    alpine:3.22 sh -c 'cp -a /seed/. /workspace/ && sync' >/dev/null 2>&1 || { log "seed $n FAILED"; return 1; }
  log "seed $n: $(dt $t0 $(now)) s"
}

wait_exec() { # name: poll until a first exec answers, for at most 60 s
  local i
  for i in {1..1200}; do container exec $1 true >/dev/null 2>&1 && return 0; sleep 0.05; done
  return 1
}

launch() { # name image cpus mem [volume]: run --detach, then time until a first exec answers
  local n=$1 img=$2 cpus=$3 mem=$4 vol=${5:-} t0 t1 t2 mounts=()
  [[ -n $vol ]] && mounts=(--mount type=volume,source=$vol,target=/workspace)
  container delete -f $n >/dev/null 2>&1
  t0=$(now)
  container run --detach --name $n --cpus $cpus --memory $mem --network none \
    --label freeside.bench=density $mounts "$img" sh -c "$MAIN" >/dev/null 2>&1 || { log "launch $n FAILED"; return 1; }
  t1=$(now)
  wait_exec $n || { log "launch $n FAILED: no exec within 60 s"; return 1; }
  t2=$(now)
  vz_lookup $n >/dev/null
  log "launch $n ($img, $cpus CPU, $mem): run-detach $(dt $t0 $t1) s, first exec $(dt $t0 $t2) s"
}

launch_create_start() { # ward's path: create, inspect, start, then first exec
  local n=$1 vol=$2 t0 t1 t2 t3 t4
  container delete -f $n >/dev/null 2>&1
  t0=$(now)
  container create --name $n --cpus $WRITER_CPUS --memory $WRITER_MEM --network none \
    --label freeside.bench=density --mount type=volume,source=$vol,target=/workspace \
    "$IMG" sh -c "$MAIN" >/dev/null 2>&1; t1=$(now)
  container inspect $n >/dev/null 2>&1; t2=$(now)
  container start $n >/dev/null 2>&1; t3=$(now)
  wait_exec $n || { log "create-start $n FAILED: no exec within 60 s"; return 1; }; t4=$(now)
  vz_lookup $n >/dev/null
  log "create-start $n: create $(dt $t0 $t1) s, inspect $(dt $t1 $t2) s, start $(dt $t2 $t3) s, first exec $(dt $t0 $t4) s"
}

teardown() {
  local n=$1 t0 t1 t2 p=$(vz_pid $1)
  t0=$(now); container stop $n >/dev/null 2>&1; t1=$(now); container delete $n >/dev/null 2>&1; t2=$(now)
  local gone=yes; [[ -n "$p" ]] && kill -0 $p 2>/dev/null && gone=no
  log "teardown $n: stop $(dt $t0 $t1) s, delete $(dt $t1 $t2) s, VM process exited: $gone"
}

start_build() {
  container exec --detach $1 sh -c 'cd /workspace/daemon && date +%s.%N > /tmp/t0 && (go build ./... && go test ./... -count=1) > /tmp/build.log 2>&1; echo $? > /tmp/exit; cat /sys/fs/cgroup/memory.peak > /tmp/memcg-peak 2>/dev/null; date +%s.%N > /tmp/t1' >/dev/null 2>&1
}
build_done() { container exec $1 test -f /tmp/t1 >/dev/null 2>&1; }
build_result() { # -> "wall_s exit memcg_peak_mib ok_pkgs fail_pkgs oom_kills"
  local n=$1
  container exec $n cat /tmp/build.log > "$OUT/build-$n-$STEP.log" 2>/dev/null
  local t0=$(container exec $n cat /tmp/t0) t1=$(container exec $n cat /tmp/t1) ex=$(container exec $n cat /tmp/exit)
  local pk=$(container exec $n cat /tmp/memcg-peak 2>/dev/null); pk=$(( ${pk:-0} / 1048576 ))
  local ok=$(grep -c "^ok" "$OUT/build-$n-$STEP.log") fail=$(grep -c $'^FAIL\t' "$OUT/build-$n-$STEP.log")
  local oom=$(container logs --boot $n 2>/dev/null | grep -c "CONSTRAINT_MEMCG")
  echo "$(dt $t0 $t1) $ex $pk $ok $fail $oom"
}

run_builds() { # names...: start together, sample until all finish or the guard trips; returns 1 on guard
  local names=("$@") n pending=1
  for n in $names; do start_build $n; done
  log "[$STEP] builds started on: $names"
  while (( pending )); do
    sample $STEP $names
    if ! guard_ok; then
      log "[$STEP] GUARD TRIPPED: pressure=${HOST[1]} disk_free_gib=${HOST[7]}; tearing down"
      for n in $names; do teardown $n; done
      return 1
    fi
    pending=0
    for n in $names; do build_done $n || pending=1; done
    (( pending )) && sleep $SAMPLE_EVERY
  done
  for n in $names; do log "[$STEP] result $n (wall exit memcg_peak_mib ok fail oom): $(build_result $n)"; done
}

guest_mem() { container exec $1 sh -c 'awk "/^(MemTotal|MemFree|MemAvailable|Cached|Shmem):/ {printf \"%s %d MiB  \", \$1, \$2/1024}" /proc/meminfo' 2>/dev/null; }

STEP=$1; N=${2:-1}
log "=== step $STEP $N; host: $(host_row) (pressure free inactive compressor swap idle diskfree foreign)"
case $STEP in
  idle)
    n=$PREFIX-idle; seed_volume $n
    for mem in 2048M 1024M; do
      launch $n "$IMG" 4 $mem $n-ws
      sleep 10; sample idle-project-$mem-10s $n; sleep 20; sample idle-project-$mem-30s $n; sleep 30; sample idle-project-$mem-60s $n
      log "guest free -m: $(guest_mem $n)"; teardown $n
    done
    if [[ -n $HELPER_IMG ]]; then
      launch $n-helper "$HELPER_IMG" 4 1024M
      sleep 10; sample idle-helper-10s $n-helper; sleep 50; sample idle-helper-60s $n-helper; teardown $n-helper
    fi
    container volume delete $n-ws >/dev/null 2>&1 ;;
  coldstart)
    n=$PREFIX-cold; seed_volume $n
    for i in 1 2 3; do launch $n "$IMG" $WRITER_CPUS $WRITER_MEM $n-ws; teardown $n; done
    for i in 1 2 3; do launch_create_start $n $n-ws; teardown $n; done
    container volume delete $n-ws >/dev/null 2>&1 ;;
  build)
    n=$PREFIX-b1; seed_volume $n; launch $n "$IMG" $WRITER_CPUS $WRITER_MEM $n-ws; sample pre-build $n
    STEP=build-1 run_builds $n || exit 1
    sleep 10; sample post-build $n; log "guest free -m post-build: $(guest_mem $n)"
    r=$(container exec $n sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches && echo ok || echo refused' 2>&1); log "drop_caches: $r"
    sleep 10; sample post-drop-caches $n; log "guest free -m post-drop: $(guest_mem $n)"
    container exec $n sh -c 'rm -rf /root/.cache/go-build /tmp/*; sync; echo 3 > /proc/sys/vm/drop_caches' >/dev/null 2>&1
    sleep 10; sample post-rm-cache $n; log "guest free -m post-rm: $(guest_mem $n)"
    sample pre-delete; teardown $n; sleep 10; sample post-delete
    container volume delete $n-ws >/dev/null 2>&1 ;;
  concurrent)
    H0=($(host_row)); (( H0[8] == 0 )) || { log "[concurrent-$N] refused: ${H0[8]} foreign containers running"; exit 2; }
    names=(); for i in $(seq 1 $N); do names+=($PREFIX-c$i); done
    for n in $names; do seed_volume $n; done
    sample pre-launch-$N
    t0=$(now); for n in $names; do launch $n "$IMG" $WRITER_CPUS $WRITER_MEM $n-ws; done
    log "launched $N wards serially in $(dt $t0 $(now)) s"
    sleep 10; sample idle-$N $names
    STEP=concurrent-$N run_builds $names; rc=$?
    if (( rc == 0 )); then
      sleep 10; sample post-build-$N $names
      t0=$(now); for n in $names; do teardown $n; done; log "tore down $N wards in $(dt $t0 $(now)) s"
    fi
    sleep 10; sample post-delete-$N
    for n in $names; do container volume delete $n-ws >/dev/null 2>&1; done
    exit $rc ;;
  cleanup)
    for c in $(container list --all --format json | jq -r '.[] | .configuration.id | select(startswith("'$PREFIX'"))'); do container delete -f $c; done
    for v in $(container volume list --format json | jq -r '.[] | .id | select(startswith("'$PREFIX'"))'); do container volume delete $v; done ;;
esac
log "=== step $STEP done"
