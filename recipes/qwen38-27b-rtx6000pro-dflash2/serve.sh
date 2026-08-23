#!/usr/bin/env bash
set -euo pipefail

MODEL_PATH="${MODEL_PATH:?MODEL_PATH is required}"
DRAFTER_PATH="${DRAFTER_PATH:?DRAFTER_PATH is required}"
SERVED="${SERVED:-qwen3.8-27b-6000pro}"
PORT="${PORT:-8888}"
MAX_RUNNING_REQUESTS="${MAX_RUNNING_REQUESTS:-8}"

INSTALLED=/sgl-workspace/sglang/python/sglang
OVERLAY_ROOT=/tmp/lmw-sglang-overlay
OVERLAY="${OVERLAY_ROOT}/sglang"
PATCH=/lmw/assets/patch/sglang

required=(
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
)

[[ -d "${INSTALLED}" ]] || { echo "missing installed SGLang package: ${INSTALLED}" >&2; exit 1; }
rm -rf "${OVERLAY_ROOT}"
mkdir -p "${OVERLAY_ROOT}"
cp -a "${INSTALLED}" "${OVERLAY}"
for rel in "${required[@]}"; do
  [[ -s "${PATCH}/${rel}" ]] || { echo "missing DFlash2 overlay: ${rel}" >&2; exit 1; }
  install -D -m 0444 "${PATCH}/${rel}" "${OVERLAY}/${rel}"
done

export PYTHONPATH="${OVERLAY_ROOT}:/sgl-workspace/sglang/python${PYTHONPATH:+:${PYTHONPATH}}"
export HF_HOME=/models/hub
export HF_HUB_OFFLINE=1
export TRANSFORMERS_OFFLINE=1
export TRITON_CACHE_DIR=/tmp/triton
mkdir -p "${TRITON_CACHE_DIR}"

exec python3 -m sglang.launch_server \
  --model-path "${MODEL_PATH}" \
  --served-model-name "${SERVED}" \
  --trust-remote-code \
  --mem-fraction-static 0.90 \
  --attention-backend flashinfer \
  --mm-feature-transport cpu \
  --chunked-prefill-size 4096 \
  --max-prefill-tokens 4096 \
  --kv-cache-dtype fp8_e4m3 \
  --context-length 262144 \
  --max-running-requests "${MAX_RUNNING_REQUESTS}" \
  --speculative-algorithm DFLASH \
  --speculative-draft-model-path "${DRAFTER_PATH}" \
  --speculative-num-draft-tokens 8 \
  --speculative-draft-model-quantization unquant \
  --speculative-draft-attention-backend flashinfer \
  --min-free-slots-delay 1 \
  --reasoning-parser qwen3 \
  --tool-call-parser qwen3_coder \
  --sampling-defaults model \
  --enable-metrics \
  --host 0.0.0.0 \
  --port "${PORT}"
