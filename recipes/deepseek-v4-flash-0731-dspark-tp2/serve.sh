#!/usr/bin/env bash
# LMW launcher for the DeepSeek V4 Flash (DSpark) two-node GB10 cluster.
# Reads all wiring (rank, master, model path, NCCL fabric, DSpark tuning) from
# the environment set by the recipe; adds --headless on non-zero ranks.
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
[[ "$NODE_RANK" == "0" ]] || headless=(--headless)

speculative_config="{\"method\":\"dspark\",\"num_speculative_tokens\":${MTP_NUM_TOKENS:-5},\"draft_sample_method\":\"probabilistic\"}"

exec /usr/local/bin/vllm serve "$MODEL_PATH" \
  --served-model-name "$SERVED" \
  --host "$API_HOST" \
  --port "$API_PORT" \
  --middleware model_capabilities.ModelCapabilitiesMiddleware \
  --trust-remote-code \
  --tensor-parallel-size 2 \
  --pipeline-parallel-size 1 \
  --kv-cache-dtype "${KV_CACHE_DTYPE:-fp8_ds_mla}" \
  --block-size 256 \
  --max-model-len "$MAX_MODEL_LEN" \
  --max-num-seqs "${MAX_NUM_SEQS:-6}" \
  --max-num-batched-tokens "${MAX_NUM_BATCHED_TOKENS:-8192}" \
  --long-prefill-token-threshold "${LONG_PREFILL_TOKEN_THRESHOLD:-0}" \
  --scheduler-cls decode_aware_scheduler.DecodeAwareScheduler \
  --max-cudagraph-capture-size "$(( ${MAX_NUM_SEQS:-6} * (${MTP_NUM_TOKENS:-5} + 1) ))" \
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.85}" \
  --enable-prefix-caching \
  --async-scheduling \
  --enable-chunked-prefill \
  --speculative-config "$speculative_config" \
  --tokenizer-mode deepseek_v4 \
  --distributed-executor-backend mp \
  --moe-backend flashinfer_b12x \
  --tool-call-parser deepseek_v4 \
  --enable-auto-tool-choice \
  --reasoning-parser deepseek_v4 \
  --reasoning-config '{"reasoning_parser":"deepseek_v4","reasoning_start_str":"\u003cthink\u003e","reasoning_end_str":"\u003c/think\u003e"}' \
  --default-chat-template-kwargs '{"thinking":false}' \
  --generation-config vllm \
  --enable-flashinfer-autotune \
  --nnodes "$WORLD_SIZE" \
  --node-rank "$NODE_RANK" \
  --master-addr "$MASTER_ADDR" \
  --master-port "$MASTER_PORT" \
  "${headless[@]}"
