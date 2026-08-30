#!/usr/bin/env bash
# Rank-local LMW launcher for GLM-5.3 Flash NVFP4 + DFlash2 on two DGX Sparks.
set -euo pipefail

for required in NODE_RANK WORLD_SIZE MASTER_ADDR MASTER_PORT LOCAL_FABRIC_ADDR \
  FABRIC_INTERFACE FABRIC_RDMA_DEVICE FABRIC_GID_INDEX MODEL_PATH \
  DFLASH_MODEL_PATH SERVED_MODEL_NAME API_PORT; do
  if [[ -z "${!required:-}" ]]; then
    echo "[glm53-nvfp4] missing required environment: ${required}" >&2
    exit 2
  fi
done

if [[ "${NODE_RANK}" != "0" && "${NODE_RANK}" != "1" ]]; then
  echo "[glm53-nvfp4] NODE_RANK must be 0 or 1" >&2
  exit 2
fi
if [[ ! -f "${MODEL_PATH}/config.json" ]]; then
  echo "[glm53-nvfp4] model snapshot is incomplete: ${MODEL_PATH}/config.json is missing" >&2
  exit 3
fi
if [[ ! -f "${DFLASH_MODEL_PATH}/model.safetensors" ]]; then
  echo "[glm53-nvfp4] DFlash2 snapshot is incomplete: ${DFLASH_MODEL_PATH}/model.safetensors is missing" >&2
  exit 3
fi

kpool_source=/lmw/assets/docker/sparse_attn_indexer_kpool_sm121.py
kpool_target=/usr/local/lib/python3.12/dist-packages/vllm/model_executor/layers/sparse_attn_indexer_kpool.py
if [[ ! -f "${kpool_source}" || ! -f "${kpool_target}" ]]; then
  echo "[glm53-nvfp4] reviewed SM121 top-k patch or target module is missing" >&2
  exit 4
fi
cp "${kpool_source}" "${kpool_target}"
chmod 0644 "${kpool_target}"

headless=()
if [[ "${NODE_RANK}" != "0" ]]; then
  headless=(--headless)
fi

speculative_config="$(python3 -S -c 'import json, os
print(json.dumps({
  "method": "dflash",
  "model": os.environ["DFLASH_MODEL_PATH"],
  "num_speculative_tokens": int(os.environ.get("DFLASH_TOKENS", "7")),
}, separators=(",", ":")))')"

args=(
  --served-model-name "${SERVED_MODEL_NAME}"
  --host 0.0.0.0
  --port "${API_PORT}"
  --trust-remote-code
  --tensor-parallel-size "${TENSOR_PARALLEL_SIZE:-2}"
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.88}"
  --max-model-len "${MAX_MODEL_LEN:-262144}"
  --max-num-seqs "${MAX_NUM_SEQS:-6}"
  --block-size "${BLOCK_SIZE:-2304}"
  --moe-backend "${MOE_BACKEND:-marlin}"
  --speculative-config "${speculative_config}"
  --kv-cache-dtype "${KV_CACHE_DTYPE:-fp8_e4m3}"
  --enforce-eager
  --tool-call-parser glm47
  --enable-auto-tool-choice
  --reasoning-parser glm45
  --default-chat-template-kwargs '{"enable_thinking":false}'
  --chat-template /lmw/assets/chat_template_mm.jinja
  --distributed-executor-backend mp
  --nnodes "${WORLD_SIZE}"
  --node-rank "${NODE_RANK}"
  --master-addr "${MASTER_ADDR}"
  --master-port "${MASTER_PORT}"
)
args+=("${headless[@]}")

echo "[glm53-nvfp4] rank ${NODE_RANK}/${WORLD_SIZE} on ${LOCAL_FABRIC_ADDR} via ${FABRIC_INTERFACE}/${FABRIC_RDMA_DEVICE} gid ${FABRIC_GID_INDEX}"
echo "[glm53-nvfp4] joining ${MASTER_ADDR}:${MASTER_PORT}; model=${MODEL_PATH}; dflash=${DFLASH_MODEL_PATH}"
echo "[glm53-nvfp4] KV cache is profiler-sized; no fixed --kv-cache-memory override is used"
exec vllm serve "${MODEL_PATH}" "${args[@]}"
