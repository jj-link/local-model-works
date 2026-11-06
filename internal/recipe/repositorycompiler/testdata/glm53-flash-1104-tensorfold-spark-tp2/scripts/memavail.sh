# The memory a Spark can give a starting rank, counted the way the engine's own startup budget counts it.
#
# GB10 is a unified-memory device, so the engine budgets from the host's memory, not a card's free bytes:
# zig/src/native/cuda_memory.zig reads /proc/meminfo's MemTotal and MemAvailable, then adds the free pages parked on
# the kernel's per-CPU lists (zig/src/native/parked_pages.zig), which MemFree and MemAvailable both leave out. This
# file mirrors that rule in shell so that ./start.sh's memory check never expects more, or less, than the engine will
# budget a moment later.
#
# A bulk free (an image build or copy, a compiler's exit, the first start of a new image) leaves 2-9 GiB on the per-CPU
# free lists for a minute or two. A check that read MemAvailable alone would report several GiB less than the engine has.
#
# The rule, the engine's: in /proc/zoneinfo, under each zone's `pagesets` line (the indented lines that follow it: four
# spaces, or the "  vm stats threshold" line), sum every `count:` (pages on a CPU's list) and every `high_min:` (that
# list's resting level); parked = count - high_min pages, or count pages less 512 MiB when the kernel prints no
# high_min (before 6.7); never below 0. Any unparsable count or high_min counts nothing, as the engine does. Then
# available = MemAvailable + parked, at most MemTotal. /proc/meminfo is read first and /proc/zoneinfo right after, so a
# page that drains in between is counted by neither, never twice.
#
# Sourced by scripts/nodes.sh; start.sh sends mem_counting to the worker over ssh, so it stays self-contained (awk and
# getconf only) and POSIX-awk clean (mawk, gawk, BSD awk).
#
# The shell rule is copied from the 2x DGX Sparks recipe's scripts/memavail.sh
# (github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold, branch zig-engine), where it mirrored the same
# rule carried as an engine patch. At TensorFold f350cd3e the rule is upstream, so nothing here is gated on a patch.

# mem_counting [meminfo] [zoneinfo] [page bytes]: "<available GiB> <parked GiB> <parked bytes>" (GiB with one
# decimal), from /proc/meminfo, /proc/zoneinfo and getconf PAGESIZE by default. A missing zoneinfo counts no parked
# pages. Prints nothing (and fails) when the meminfo has no MemAvailable.
mem_counting() {
  local mi=${1:-/proc/meminfo} zi=${2:-/proc/zoneinfo} pg=${3:-}
  [ -n "$pg" ] || pg=$(getconf PAGESIZE 2>/dev/null)
  case "$pg" in ''|*[!0-9]*) pg=4096 ;; esac
  [ -r "$zi" ] || zi=/dev/null
  awk -v page="$pg" -v rest_default=536870912 -v zfile="$zi" '
    FILENAME != zfile {
      if ($1 == "MemTotal:") total = $2 * 1024
      else if ($1 == "MemAvailable:") { avail = $2 * 1024; seen = 1 }
      next
    }
    {
      row = $0
      gsub(/\r/, " ", row)
      t = row; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t)
      if (t == "pagesets") { listed = 1; next }
      if (listed && substr($0, 1, 4) != "    " && substr($0, 1, 10) != "  vm stats") { listed = 0; next }
      if (!listed) next
      n = split(t, w, /[ \t]+/)
      if (w[1] != "count:" && w[1] != "high_min:") next
      if (n < 2 || w[2] !~ /^[0-9]+$/) { bad = 1; next }
      if (w[1] == "count:") count += w[2]; else rest += w[2]
    }
    END {
      if (!seen) exit 1
      parked = 0
      if (!bad && count > 0) {
        resting = rest > 0 ? rest * page : rest_default
        parked = count * page - resting
        if (parked < 0) parked = 0
      }
      a = avail + parked
      if (total > 0 && a > total) a = total
      printf "%.1f %.1f %.0f\n", a / 1073741824, parked / 1073741824, parked
    }' "$mi" "$zi"
}

# mem_avail_gb: "<GiB available, one decimal> <GiB parked>" on this node, for start.sh's memory check.
# Falls back to `free`'s available column when /proc/meminfo has no MemAvailable.
mem_avail_gb() {
  local m
  if m=$(mem_counting 2>/dev/null) && [ -n "$m" ]; then
    echo "$m" | awk '{ printf "%s %s\n", $1, $2 }'
  else
    free -g 2>/dev/null | awk '/^Mem:/ { printf "%s.0 0.0\n", $7 }'
  fi
}

# mem_total_gb: MemTotal in GiB, one decimal.
mem_total_gb() {
  awk '$1 == "MemTotal:" { printf "%.1f\n", $2 * 1024 / 1073741824 }' /proc/meminfo 2>/dev/null
}
