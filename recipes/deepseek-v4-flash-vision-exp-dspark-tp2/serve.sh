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

# --- Vision-Exp boot patch chain (upstream compose order, curated subset) ---
# 1) Encoder copy: the image's stock vLLM encoder has no image-placeholder
#    support; Vision-Exp requires the checkpoint's encoding_dsv4.py.
encoding_source=""
for candidate in "$MODEL_PATH/encoding/encoding_dsv4.py" "$MODEL_PATH"/snapshots/*/encoding/encoding_dsv4.py; do
  if [[ -f "$candidate" ]]; then
    encoding_source="$candidate"
    break
  fi
done
[[ -n "$encoding_source" ]] || { echo "FATAL: encoding_dsv4.py not found under $MODEL_PATH" >&2; exit 1; }
cp "$encoding_source" /usr/local/lib/python3.12/dist-packages/vllm/tokenizers/deepseek_v4_encoding.py
# Reasoning-effort low mapping (verbatim from upstream compose, fail-closed).
python3 -c 'from pathlib import Path; p=Path("/usr/local/lib/python3.12/dist-packages/vllm/tokenizers/deepseek_v4.py"); s=p.read_text(); old="elif reasoning_effort in (\"max\", \"xhigh\"):\n                reasoning_effort = \"max\"\n            else:\n                reasoning_effort = \"high\""; new="elif reasoning_effort in (\"max\", \"xhigh\"):\n                reasoning_effort = \"max\"\n            elif reasoning_effort == \"high\":\n                reasoning_effort = \"high\"\n            else:\n                reasoning_effort = \"low\""; updated=s.replace(old,new); assert new in updated; p.write_text(updated)'
python3 /lmw/assets/patches/hotfix-encoding-dsv4-issue21.py
# 2) Vision + encoder-output hotfixes (both ranks patch their own image layer).
python3 /lmw/assets/patches/hotfix-dsv4-vision-exp.py /lmw/assets/patches/vision_exp
python3 /lmw/assets/patches/hotfix-vllm-empty-encoder-output.py

speculative_config="{\"method\":\"dspark\",\"num_speculative_tokens\":${MTP_NUM_TOKENS:-6},\"draft_sample_method\":\"probabilistic\"}"

exec /usr/local/bin/vllm serve "$MODEL_PATH" \
  --served-model-name "$SERVED" \
  --host "$API_HOST" \
  --port "$API_PORT" \
  --middleware model_capabilities.ModelCapabilitiesMiddleware \
  --trust-remote-code \
  --tensor-parallel-size 2 \
  --pipeline-parallel-size 1 \
  --kv-cache-dtype "${KV_CACHE_DTYPE:-nvfp4_ds_mla}" \
  --block-size 256 \
  --max-model-len "$MAX_MODEL_LEN" \
  --max-num-seqs "${MAX_NUM_SEQS:-6}" \
  --max-num-batched-tokens "${MAX_NUM_BATCHED_TOKENS:-8192}" \
  --long-prefill-token-threshold "${LONG_PREFILL_TOKEN_THRESHOLD:-1024}" \
  --scheduler-cls decode_aware_scheduler.DecodeAwareScheduler \
  --max-cudagraph-capture-size "$(( ${MAX_NUM_SEQS:-6} * (${MTP_NUM_TOKENS:-6} + 1) ))" \
  --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.85}" \
  --enable-prefix-caching \
  --async-scheduling \
  --enable-chunked-prefill \
  --speculative-config "$speculative_config" \
  --tokenizer-mode deepseek_v4 \
  --limit-mm-per-prompt '{"image":8}' \
  --distributed-executor-backend mp \
  --moe-backend flashinfer_b12x \
  --tool-call-parser deepseek_v4 \
  --enable-auto-tool-choice \
  --reasoning-parser deepseek_v4 \
  --reasoning-config '{"reasoning_parser":"deepseek_v4","reasoning_start_str":"\u003cthink\u003e","reasoning_end_str":"\u003c/think\u003e"}' \
  --default-chat-template-kwargs '{"thinking":true,"reasoning_effort":"max"}' \
  --generation-config vllm \
  --enable-flashinfer-autotune \
  --nnodes "$WORLD_SIZE" \
  --node-rank "$NODE_RANK" \
  --master-addr "$MASTER_ADDR" \
  --master-port "$MASTER_PORT" \
  "${headless[@]}"
