import type { Recipe } from "~/lib/api";

const parameterLabels: Record<string, string> = {
  model_id: "Model",
  model_path: "Model location",
  served_model_name: "Served model name",
  max_concurrent_requests: "Concurrent requests",
  max_model_len: "Context limit",
  context_length: "Context length",
  gpu_memory_utilization: "GPU memory fraction",
  max_num_seqs: "Concurrent sequences",
  max_num_batched_tokens: "Tokens per batch",
  tensor_parallel_size: "Tensor parallel size",
  pipeline_parallel_size: "Pipeline parallel size",
  quant: "Quantization",
  quantization: "Quantization",
  dtype: "Precision",
  kv_cache_dtype: "KV cache format",
  api_host: "Listen address",
  api_port: "API port",
  host: "Listen address",
  port: "Port",
  worker_host: "Worker host",
  worker_ssh: "Worker SSH connection",
  worker_user: "Worker user",
  worker_home: "Worker home directory",
  worker_directory: "Worker directory",
  worker_hf_cache: "Worker Hugging Face cache",
};

export function parameterLabel(name: string): string {
  const known = parameterLabels[name.toLowerCase()];
  if (known) return known;
  const words = name.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/[_-]+/g, " ").trim().toLowerCase();
  return words ? words[0].toUpperCase() + words.slice(1) : "Setting";
}

export function recipeDisplayName(recipe: Recipe): string {
  const identity = [recipe.model, recipe.name].filter(Boolean).join(" ");
  const model = (recipe.model?.trim() || recipe.name.trim()).split("/").pop() || "Recipe";
  const rtx = /(?:rtx[-_ ]?)?6000[-_ ]?pro/i.test(identity);
  const spark = /(?:dgx[-_ ]?)?d?spark/i.test(identity);
  const parallel = identity.match(/(?:^|[-_ ])tp(\d+)(?=$|[-_ ])/i)?.[1];
  const hardware = rtx ? "RTX 6000 Pro" : spark ? `DGX Spark${parallel ? ` · TP${parallel}` : ""}` : "";
  const name = model
    .replace(/(?:[-_ ](?:rtx[-_ ]?)?6000[-_ ]?pro|[-_ ](?:dgx[-_ ]?)?d?spark)(?:[-_ ]tp\d+)?/gi, "")
    .replace(/qwen/gi, "Qwen")
    .replace(/deepseek/gi, "DeepSeek")
    .replace(/glm/gi, "GLM")
    .replace(/sglang/gi, "SGLang")
    .replace(/vllm/gi, "vLLM")
    .replace(/(\d+(?:\.\d+)?)b\b/gi, "$1B")
    .replace(/\b(flash|next|vision|spark)\b/gi, (word) => word[0].toUpperCase() + word.slice(1).toLowerCase())
    .replace(/\b(nvfp4|bf16|fp8|mtp|exl3|tp\d+)\b/gi, (word) => word.toUpperCase())
    .replace(/_/g, " ")
    .replace(/^[- ]+|[- ]+$/g, "");
  return [name || "Recipe", hardware].filter(Boolean).join(" · ");
}
