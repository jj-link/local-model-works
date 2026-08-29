#!/usr/bin/env bash
# Qwen3.8-27B on SGLang (RTX PRO 6000, 96GB, SM120/Blackwell) with DFlash2.
#
# This asset:
#   1. Copies the image's installed sglang package into a writable overlay and
#      overwrites it with the DFlash2 backport files (the pinned image predates
#      DFlash2 and ships only the DFlash1 model class).
#   2. Puts the overlay first on PYTHONPATH so the patched modules shadow the
#      stock ones.
#   3. Resolves each model cache root to its pinned snapshot and execs
#      sglang.launch_server with the repo's serving configuration.
#
# Args (from the recipe workload): --model, --drafter, --served, --port.
set -euo pipefail

MODEL=""
DRAFTER=""
SERVED="qwen3.8-27b-6000pro"
PORT="8000"
while [ $# -gt 0 ]; do
  case "$1" in
    --model)   MODEL="${2:?}";   shift 2 ;;
    --drafter) DRAFTER="${2:?}"; shift 2 ;;
    --served)  SERVED="${2:?}";  shift 2 ;;
    --port)    PORT="${2:?}";    shift 2 ;;
    *) echo "serve.sh: unknown arg $1" >&2; exit 2 ;;
  esac
done
[ -n "$MODEL" ] || { echo "serve.sh: --model required" >&2; exit 2; }
[ -n "$DRAFTER" ] || { echo "serve.sh: --drafter required" >&2; exit 2; }

# --- resolve each model cache root to its pinned snapshot -----------------
# ${artifact.*.path} is the models--owner--repo cache root (blobs/refs/
# snapshots). SGLang needs the snapshot dir containing config.json; select the
# sole snapshot and validate it before launch.
resolve_snapshot() {
  local root="$1" name="$2"
  if [ -f "$root/config.json" ]; then
    echo "$root"; return
  fi
  local snap
  snap=$(find "$root/snapshots" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | grep -v '^$')
  local count
  count=$(printf '%s\n' "$snap" | grep -c . || true)
  if [ "$count" != "1" ] || [ -z "$snap" ]; then
    echo "serve.sh: $name: expected exactly one snapshot under $root/snapshots (found $count)" >&2
    return 1
  fi
  [ -f "$snap/config.json" ] || { echo "serve.sh: $name: snapshot $snap lacks config.json" >&2; return 1; }
  echo "$snap"
}
MODEL_SNAP=$(resolve_snapshot "$MODEL" base) || exit 5
DRAFTER_SNAP=$(resolve_snapshot "$DRAFTER" drafter) || exit 5

# --- build the DFlash2 overlay (full package copy + patched files) --------
PATCH_SRC=/lmw/assets/patch/sglang
SGPKG=$(python3 -c "import sglang, os; print(os.path.dirname(sglang.__file__))")
if [ -z "$SGPKG" ] || [ ! -d "$SGPKG" ]; then
  echo "serve.sh: cannot locate installed sglang package" >&2
  exit 3
fi
OVL=/tmp/sglang-ovl
rm -rf "$OVL"
mkdir -p "$OVL/sglang"
# Copy the full regular package so unpatched modules resolve; then overwrite
# with the DFlash2 backport files.
cp -a "$SGPKG/." "$OVL/sglang/"
FILES="
srt/models/dflash.py
kernels/ops/speculative/dflash.py
srt/speculative/dflash_utils.py
srt/speculative/dflash_worker_v2.py
srt/speculative/dflash_info.py
srt/speculative/dflash_info_v2.py
srt/speculative/draft_worker_common.py
srt/speculative/spec_utils.py
srt/mem_cache/allocation_sizing.py
srt/layers/moe/utils.py
srt/layers/logprob_processor.py
"
while IFS= read -r f; do
  [ -z "$f" ] && continue
  src="$PATCH_SRC/$f"
  dest="$OVL/sglang/$f"
  [ -s "$src" ] || { echo "serve.sh: missing overlay source $src" >&2; exit 4; }
  mkdir -p "$(dirname "$dest")"
  cp "$src" "$dest"
done <<< "$FILES"

# Prepend the overlay's PARENT so its sglang/ package shadows the stock one on
# PYTHONPATH; keep the stock package parent as a fallback.
STOCK_PARENT=$(dirname "$SGPKG")
export PYTHONPATH="$OVL:$STOCK_PARENT${PYTHONPATH:+:$PYTHONPATH}"

export MAX_MODEL_LEN=262144
export DGX_MODEL_CAPABILITIES_PATH=/lmw/assets/assets/capabilities.json

exec python3 -m sglang.launch_server \
  --model-path "$MODEL_SNAP" \
  --served-model-name "$SERVED" \
  --trust-remote-code \
  --mem-fraction-static 0.90 \
  --attention-backend flashinfer \
  --chunked-prefill-size 4096 \
  --max-prefill-tokens 4096 \
  --kv-cache-dtype fp8_e4m3 \
  --context-length 262144 \
  --mm-feature-transport cpu \
  --max-running-requests 8 \
  --speculative-algorithm DFLASH \
  --speculative-draft-model-path "$DRAFTER_SNAP" \
  --speculative-num-draft-tokens 8 \
  --speculative-draft-model-quantization unquant \
  --speculative-draft-attention-backend flashinfer \
  --min-free-slots-delay 1 \
  --reasoning-parser qwen3 \
  --tool-call-parser qwen3_coder \
  --sampling-defaults model \
  --admin-api-key sk-local-admin \
  --host 0.0.0.0 \
  --port "$PORT"
