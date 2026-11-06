#!/usr/bin/env bash
# Stop both ranks, save one gzipped archive with their logs and keep the newest LOG_KEEP (10).
# Rank 0 goes first: it drains its requests and ends the follower's steps; then rank 1 on the worker.
# A rank that still runs after the STOP_TIMEOUT stop gets docker kill; one still running after the kill
# fails the stop, so a start aborts for a human instead of layering a second container onto a wedged
# one (two ~100 GiB workloads on a 128 GB host wedged spark4 on 2026-11-03).
# Usage: ./stop.sh; DRY_RUN=1 ./stop.sh prints the plan and changes nothing.
# Only this recipe's containers (label tf.recipe=glm53-spark) are stopped; any other container is left alone.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
# shellcheck source=scripts/prepare.sh
source ./scripts/prepare.sh
[[ "$STOP_TIMEOUT" =~ ^[1-9][0-9]*$ && "$LOG_KEEP" =~ ^[1-9][0-9]*$ ]] || fail E_CONFIG 'STOP_TIMEOUT and LOG_KEEP must be positive integers.'
[[ "$CONTAINER_NAME" != tensorfold-zig-r0 && "$CONTAINER_NAME" != tensorfold-zig-r1 && "$CONTAINER_NAME" != dsv41-1017-exl3-tf ]] \
  || fail E_CONFIG "$CONTAINER_NAME is a DeepSeek recipe's container on these Sparks; this script never stops it."
# A warning only. /health's live.connections counts the requests running and waiting; a count it cannot read is not none.
warn_open_requests() {
  local host="$HOST" open
  [[ "$host" != 0.0.0.0 && "$host" != :: ]] || host=127.0.0.1
  [[ "$host" != *:* ]] || host="[$host]"
  open=$(curl -s --max-time 3 -w '\n%{http_code}' "http://$host:$PORT/health" 2>/dev/null | python3 -c '
import json, sys
body, _, code = sys.stdin.read().rpartition("\n")
r = json.loads(body) if code == "200" else {}
n = r["live"]["connections"] if type(r) is dict and r.get("error") is None else None
sys.exit(1) if type(n) is not int or n < 0 else print(n)' 2>/dev/null) || open='?'
  if [[ "$open" == '?' ]]; then
    warn W_OPEN 'Could not read the open requests from /health; any still open get TF_DRAIN_S to finish, then are cut off.'
  elif (( open > 0 )); then
    warn W_OPEN "$open request(s) open; they get TF_DRAIN_S to finish, then are cut off."
  fi
}
if [[ "$DRY_RUN" == 1 ]]; then
  log "Stop each rank only if its container carries the label tf.recipe=glm53-spark, rank 0 first (up to $STOP_TIMEOUT s each):"
  plan docker stop -t "$STOP_TIMEOUT" "$CONTAINER_NAME"
  plan ssh "$WORKER" docker stop -t "$STOP_TIMEOUT" "$CONTAINER_NAME"
  log "  Save both ranks' Docker output as $LOG_DIR/glm53-spark-<timestamp>.tar.gz; keep the newest $LOG_KEEP archives."
  plan docker rm "$CONTAINER_NAME"
  plan ssh "$WORKER" docker rm "$CONTAINER_NAME"
  exit 0
fi
command -v docker >/dev/null || fail E_DEPENDENCY 'Missing command: docker.'
docker info >/dev/null 2>&1 || fail E_DOCKER 'Cannot talk to Docker. Start the daemon and check socket permissions.'
if [[ "${GLM_STOP_LOCK_HELD:-0}" != 1 ]]; then
  mkdir -p "$STATE_DIR"
  exec 8>"$STATE_DIR/start.lock"
  flock -n 8 || fail E_LOCK 'Another start, stop or preparation is in progress.'
fi

here=0 there=0
if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then here=1; fi
if [[ -n "$WORKER" ]] && wssh docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then there=1; fi
if (( here == 0 && there == 0 )); then
  log "No container named $CONTAINER_NAME on either Spark. Nothing to stop."; exit 0
fi

# Each side is checked for this recipe's label before anything is stopped.
if (( here )); then
  [[ "$(docker inspect -f '{{index .Config.Labels "tf.recipe"}}' "$CONTAINER_NAME")" == glm53-spark ]] \
    || fail E_STOP "$CONTAINER_NAME here was not started by this recipe (no tf.recipe=glm53-spark label); not touching it."
fi
if (( there )); then
  [[ "$(wssh docker inspect -f '{{index .Config.Labels "tf.recipe"}}' "$CONTAINER_NAME" 2>/dev/null || true)" == glm53-spark ]] \
    || fail E_STOP "$CONTAINER_NAME on $(worker_host) was not started by this recipe; not touching it."
fi

RUN_LOG_DIR=$(readlink -f "$LOG_DIR/current" 2>/dev/null || true)
[[ -n "$RUN_LOG_DIR" && -d "$RUN_LOG_DIR" ]] || { RUN_LOG_DIR="$LOG_DIR/run-stopped-$(date -u +%Y%m%dT%H%M%SZ)-$$"; mkdir -p "$RUN_LOG_DIR"; }

if (( here )) && [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME")" == true ]]; then
  warn_open_requests
  log "Stopping rank 0 here (up to $STOP_TIMEOUT s; requests in progress get TF_DRAIN_S to finish)."
  docker stop -t "$STOP_TIMEOUT" "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" == true ]]; then
    warn W_STOP "Rank 0 still runs after the ${STOP_TIMEOUT}s stop; docker kill."
    docker kill "$CONTAINER_NAME" >/dev/null 2>&1 || true
  fi
  if [[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" == true ]]; then
    fail E_STOP "Rank 0 still runs after stop and kill. A human must clean it up (docker ps; docker rm -f $CONTAINER_NAME) before a start can be trusted."
  fi
fi
if (( there )); then
  log "Stopping rank 1 on $(worker_host)."
  wssh docker stop -t "$STOP_TIMEOUT" "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if [[ "$(wssh docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" == true ]]; then
    warn W_STOP "Rank 1 still runs on $(worker_host) after the ${STOP_TIMEOUT}s stop; docker kill."
    wssh docker kill "$CONTAINER_NAME" >/dev/null 2>&1 || true
  fi
  if [[ "$(wssh docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" == true ]]; then
    fail E_STOP "Rank 1 on $(worker_host) still runs after stop and kill. A human must clean it up there (docker ps; docker rm -f $CONTAINER_NAME)."
  fi
fi

# Both ranks' output, saved before the containers go.
mkdir -p "$LOG_DIR"
(( here == 0 )) || docker logs --timestamps "$CONTAINER_NAME" > "$RUN_LOG_DIR/rank0.log" 2>&1 || warn W_LOG 'Could not save rank 0 output.'
(( here == 0 )) || docker inspect "$CONTAINER_NAME" > "$RUN_LOG_DIR/rank0.json" 2>/dev/null || true
(( there == 0 )) || wssh docker logs --timestamps "$CONTAINER_NAME" > "$RUN_LOG_DIR/rank1.log" 2>&1 || warn W_LOG 'Could not save rank 1 output.'
(( there == 0 )) || wssh docker inspect "$CONTAINER_NAME" > "$RUN_LOG_DIR/rank1.json" 2>/dev/null || true

archive="$LOG_DIR/glm53-spark-$(date -u +%Y%m%dT%H%M%SZ)-$$.tar.gz"
tar -czf "$archive.tmp" -C "$RUN_LOG_DIR" . || fail E_LOG 'Could not archive logs. Check LOG_DIR space and permissions; the stopped containers were retained.'
mv -- "$archive.tmp" "$archive"
(( here == 0 )) || docker rm "$CONTAINER_NAME" >/dev/null || fail E_STOP 'Logs were saved, but Docker could not remove rank 0.'
(( there == 0 )) || wssh docker rm "$CONTAINER_NAME" >/dev/null 2>&1 \
  || warn W_STOP "Logs were saved, but rank 1 on $(worker_host) could not be removed; remove it there with: docker rm $CONTAINER_NAME"
# Raw logs are deleted only after a successful archive and only in a recipe-owned run directory.
if [[ "$RUN_LOG_DIR" == "$LOG_DIR"/run-* && ! -L "$RUN_LOG_DIR" ]]; then
  rm -rf -- "$RUN_LOG_DIR"
  [[ ! -L "$LOG_DIR/current" || "$(readlink -- "$LOG_DIR/current")" != "$RUN_LOG_DIR" ]] || rm -- "$LOG_DIR/current"
fi
python3 - "$LOG_DIR" "$LOG_KEEP" <<'PY'
from pathlib import Path
import sys
archives = sorted(Path(sys.argv[1]).glob("glm53-spark-*.tar.gz"), key=lambda p: p.stat().st_mtime_ns, reverse=True)
for path in archives[int(sys.argv[2]):]:
    path.unlink()
PY
# The worker's env file can hold a key you exported, so it does not outlive the ranks.
[[ -z "$WORKER" ]] || wssh rm -f "$WORKER_STATE_DIR/rank.env" >/dev/null 2>&1 || true
log "Stopped both ranks. Logs saved to $archive (newest $LOG_KEEP kept)."
