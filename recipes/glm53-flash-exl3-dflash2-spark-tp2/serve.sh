#!/usr/bin/env bash
# Rank-local LMW launcher for GLM-5.3 Flash EXL3 on two DGX Sparks.
set -euo pipefail

for required in NODE_RANK WORLD_SIZE MASTER_ADDR MASTER_PORT LOCAL_FABRIC_ADDR \
  FABRIC_INTERFACE FABRIC_RDMA_DEVICE FABRIC_GID_INDEX MODEL_PATH \
  DFLASH_MODEL_PATH SERVED_MODEL_NAME API_PORT; do
  if [[ -z "${!required:-}" ]]; then
    echo "[glm53] missing required environment: ${required}" >&2
    exit 2
  fi
done

if [[ ! -f "${MODEL_PATH}/config.json" ]]; then
  echo "[glm53] model snapshot is incomplete: ${MODEL_PATH}/config.json is missing" >&2
  exit 3
fi
if [[ ! -f "${DFLASH_MODEL_PATH}/model.safetensors" ]]; then
  echo "[glm53] DFlash2 snapshot is incomplete: ${DFLASH_MODEL_PATH}/model.safetensors is missing" >&2
  exit 3
fi

for patch in \
  patch_glm_video_placeholders.py \
  patch_suppress_stops_in_reasoning.py \
  patch_scheduler_decode_floor.py \
  patch_glm5_drafter_group.py \
  patch_hybrid_prefix_hit.py; do
  python3 "/lmw/assets/overlay/${patch}"
done

headless=()
if [[ "${NODE_RANK}" != "0" ]]; then
  headless=(--headless)
fi

speculative_config="$(python3 -S -c 'import json, os
print(json.dumps({
  "method": "dflash",
  "model": os.environ["DFLASH_MODEL_PATH"],
  "num_speculative_tokens": int(os.environ.get("DFLASH_TOKENS", "7")),
  "kv_cache_dtype": "auto",
  "draft_sample_method": "probabilistic",
  "rejection_sample_method": "standard",
  "draft_tensor_parallel_size": int(os.environ.get("DFLASH_DRAFT_TP", "1")),
}, separators=(",", ":")))')"

args=(
  --served-model-name "${SERVED_MODEL_NAME}"
  --host 0.0.0.0
  --port "${API_PORT}"
  --tensor-parallel-size "${TENSOR_PARALLEL_SIZE:-2}"
  --nnodes "${WORLD_SIZE}"
  --node-rank "${NODE_RANK}"
  --master-addr "${MASTER_ADDR}"
  --master-port "${MASTER_PORT}"
  --distributed-executor-backend mp
  --quantization "${QUANTIZATION:-exl3}"
  --max-model-len "${MAX_MODEL_LEN:-1000000}"
  --max-num-seqs "${MAX_NUM_SEQS:-4}"
  --max-num-batched-tokens "${MAX_NUM_BATCHED_TOKENS:-1024}"
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.87}"
  --kv-cache-dtype "${KV_CACHE_DTYPE:-fp8}"
  --speculative-config "${speculative_config}"
  --chat-template /lmw/assets/files/chat_template.jinja
  --tool-call-parser glm47
  --enable-auto-tool-choice
  --reasoning-parser glm45
  --enable-prefix-caching
  --no-enable-flashinfer-autotune
)

limit_mm="${LIMIT_MM:-}"
if [[ -z "${limit_mm}" ]]; then
  limit_mm='{"image":4,"video":1}'
fi
if [[ "${LANGUAGE_MODEL_ONLY:-0}" == "1" ]]; then
  args+=(--language-model-only)
else
  args+=(--limit-mm-per-prompt "${limit_mm}")
  if [[ "${SKIP_MM_PROFILING:-1}" == "1" ]]; then
    args+=(--skip-mm-profiling)
  fi
fi
if [[ "${ENFORCE_EAGER:-0}" == "1" ]]; then
  args+=(--enforce-eager)
fi
args+=("${headless[@]}")

if [[ "${NODE_RANK}" == "0" && "${GLM53_BOOT_SHAPE_WARMUP:-1}" == "1" ]]; then
  (
    for _ in $(seq 1 360); do
      if curl -fsS --max-time 2 "http://127.0.0.1:${API_PORT}/health" >/dev/null 2>&1; then
        GLM53_WARMUP_MAX_CONCURRENCY="${MAX_NUM_SEQS:-4}" \
        GLM53_WARMUP_DFLASH_K="${DFLASH_TOKENS:-7}" \
          bash /lmw/assets/scripts/boot-shape-warmup.sh \
            "http://127.0.0.1:${API_PORT}" "${SERVED_MODEL_NAME}" \
          || echo "[glm53] boot-shape warmup incomplete; first requests may compile kernels" >&2
        exit
      fi
      sleep 5
    done
    echo "[glm53] API did not become ready before the background warmup deadline" >&2
  ) &
fi

echo "[glm53] rank ${NODE_RANK}/${WORLD_SIZE} on ${LOCAL_FABRIC_ADDR} via ${FABRIC_INTERFACE}/${FABRIC_RDMA_DEVICE} gid ${FABRIC_GID_INDEX}"
echo "[glm53] joining ${MASTER_ADDR}:${MASTER_PORT}; model=${MODEL_PATH}; dflash=${DFLASH_MODEL_PATH}"
exec vllm serve "${MODEL_PATH}" "${args[@]}"
