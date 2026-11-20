#!/bin/sh
# Measure a running container FROM OUTSIDE the measured process.
#
#   bench/measure.sh <container> <duration_s> <interval_s> <out.tsv>
#
# Writes one TSV line per sample (header included):
#   t_s  vmrss  vmhwm  rssanon  rssfile  mem_current  anon  file  inactive_file  usage_usec
# (bytes, except t_s = /proc/uptime seconds and usage_usec = cgroup cpu.stat)
# and "<out.tsv>.mode" containing "direct" or "helper".
#
# direct: Linux host that can see the container's PID in /proc.
# helper: Docker Desktop (macOS/Windows, or Docker Desktop for Linux) and rootless
#   Docker, where host /proc cannot see container PIDs. The same script then runs in
#   `docker run --pid=host --cgroupns=host --privileged alpine`. Results from the
#   helper path are INFORMATIONAL only (VM kernel, shared resources).
#
# Requires cgroup v2. Nothing is sent to the measured process.
set -eu

sample_loop() { # pid duration interval
  pid=$1; duration=$2; interval=$3
  cg=$(sed -n 's/^0:://p' "/proc/$pid/cgroup")
  dir="/sys/fs/cgroup$cg"
  [ -r "$dir/memory.current" ] || { echo "measure: cgroup v2 files not found at $dir" >&2; exit 3; }
  printf 't_s\tvmrss\tvmhwm\trssanon\trssfile\tmem_current\tanon\tfile\tinactive_file\tusage_usec\n'
  n=$((duration / interval + 1)); i=0
  while [ "$i" -lt "$n" ]; do
    t=$(cut -d' ' -f1 /proc/uptime)
    proc=$(awk '/^VmRSS:/{r=$2*1024} /^VmHWM:/{h=$2*1024} /^RssAnon:/{a=$2*1024} /^RssFile:/{f=$2*1024} END{printf "%d\t%d\t%d\t%d", r,h,a,f}' "/proc/$pid/status") || exit 4
    cur=$(cat "$dir/memory.current")
    mem=$(awk '$1=="anon"{a=$2} $1=="file"{f=$2} $1=="inactive_file"{i=$2} END{printf "%d\t%d\t%d", a,f,i}' "$dir/memory.stat")
    cpu=$(awk '$1=="usage_usec"{print $2}' "$dir/cpu.stat")
    printf '%s\t%s\t%s\t%s\t%s\n' "$t" "$proc" "$cur" "$mem" "$cpu"
    i=$((i + 1))
    if [ "$i" -lt "$n" ]; then sleep "$interval"; fi
  done
}

if [ "${1:-}" = "--sample" ]; then # internal: runs on the measuring side
  shift; sample_loop "$@"; exit 0
fi

[ $# -eq 4 ] || { echo "usage: $0 <container> <duration_s> <interval_s> <out.tsv>" >&2; exit 2; }
container=$1; duration=$2; interval=$3; out=$4
pid=$(docker inspect -f '{{.State.Pid}}' "$container")
cid=$(docker inspect -f '{{.Id}}' "$container")
[ "$pid" -gt 0 ] || { echo "measure: $container is not running" >&2; exit 2; }

# direct only when this host's /proc really is the container's host: the PID's
# cgroup path must contain the full container ID. Otherwise (Docker Desktop, a
# VM-backed daemon, a PID collision) use the helper; helper results are always
# informational.
if [ "$(uname -s)" = Linux ] && [ -r "/proc/$pid/status" ] && grep -q "$cid" "/proc/$pid/cgroup" 2>/dev/null; then
  echo direct >"$out.mode"
  sh "$0" --sample "$pid" "$duration" "$interval" >"$out"
else
  echo helper >"$out.mode"
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  docker run --rm --pid=host --cgroupns=host --privileged -v "$self:/measure.sh:ro" alpine:3@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf \
    sh /measure.sh --sample "$pid" "$duration" "$interval" >"$out"
fi
