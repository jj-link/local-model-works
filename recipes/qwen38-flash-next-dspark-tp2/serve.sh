#!/usr/bin/env bash
# LMW launcher for Qwen3.8-Flash-Next (NVFP4) on the two-node GB10 cluster.
# Mirrors upstream start.sh's docker-run contract: worker rank starts first
# (LMW startOrder), head serves the API. Applies the upstream PLE FP8 patch
# by copying it over the image's ple_layer.py (rootfs.write permission;
# upstream bind-mounts the same file read-only).
set -euo pipefail

: "${NODE_RANK:?NODE_RANK required}"
: "${WORLD_SIZE:?WORLD_SIZE required}"
: "${MASTER_ADDR:?MASTER_ADDR required}"
: "${MASTER_PORT:?MASTER_PORT required}"
: "${MODEL_PATH:?MODEL_PATH required}"
: "${SERVED:?SERVED required}"
: "${API_HOST:?API_HOST required}"
: "${API_PORT:?API_PORT required}"
: "${MAX_MODEL_LEN:?MAX_MODEL_LEN required}"

headless=()
api_args=(--host "$API_HOST" --port "$API_PORT")
[[ "$NODE_RANK" == "0" ]] || { headless=(--headless); api_args=(); }

# --- PLE FP8 patch (required for this NVFP4 checkpoint) ----------------------
PLE_TARGET=/usr/local/lib/python3.12/dist-packages/vllm/models/qwen3_8_flash_next/nvidia/ple_layer.py
[[ -f "$PLE_TARGET" ]] || { echo "FATAL: $PLE_TARGET missing (image drift)" >&2; exit 1; }
[[ -f /lmw/assets/files/ple_layer_patched.py ]] || { echo "FATAL: PLE patch asset missing" >&2; exit 1; }
cp /lmw/assets/files/ple_layer_patched.py "$PLE_TARGET"

mtp_tokens="${MTP_NUM_SPECULATIVE_TOKENS:-3}"
speculative_args=()
if [[ "$mtp_tokens" -gt 0 ]]; then
  speculative_args=(--speculative-config "{\"method\":\"mtp\",\"num_speculative_tokens\":${mtp_tokens}}")
fi

yarn_args=()
if [[ "$MAX_MODEL_LEN" -gt 262144 ]]; then
  yarn_args=(--hf-overrides "{\"rope_parameters\":{\"rope_type\":\"yarn\",\"factor\":${YARN_FACTOR:-4.0},\"original_max_position_embeddings\":262144}}")
fi

exec /usr/local/bin/vllm serve "$MODEL_PATH" \
  --served-model-name "$SERVED" \
  "${api_args[@]}" \
  --tensor-parallel-size 2 \
  --kv-cache-dtype "${KV_CACHE_DTYPE:-auto}" \
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.835}" \
  --max-model-len "$MAX_MODEL_LEN" \
  --max-num-seqs "${MAX_NUM_SEQS:-8}" \
  --max-num-batched-tokens "${MAX_NUM_BATCHED_TOKENS:-8192}" \
  --load-format safetensors \
  --safetensors-load-strategy lazy \
  --enable-chunked-prefill \
  --reasoning-parser qwen3 \
  --enable-auto-tool-choice \
  --tool-call-parser qwen3_coder \
  --distributed-executor-backend mp \
  --enable-expert-parallel \
  --all2all-backend allgather_reducescatter \
  "${speculative_args[@]}" \
  --compilation-config '{"mode":0,"cudagraph_mode":"FULL_DECODE_ONLY"}' \
  --nnodes "$WORLD_SIZE" \
  --node-rank "$NODE_RANK" \
  --master-addr "$MASTER_ADDR" \
  --master-port "$MASTER_PORT" \
  "${yarn_args[@]}" \
  "${headless[@]}"
