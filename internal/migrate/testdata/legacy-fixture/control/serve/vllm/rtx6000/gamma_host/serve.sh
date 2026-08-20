#!/usr/bin/env bash
set -euo pipefail
MODEL="${MODEL_PATH:?MODEL_PATH is required}"
SERVED="${SERVED:?SERVED is required}"
exec vllm serve "$MODEL" --served-model-name "$SERVED" --host 0.0.0.0 --port 8000 --quantization awq
