# DFlash 2 backport overlay for the pinned SGLang image

This directory is a **read-only, git-tracked overlay** that adds DFlash 2 support
to the SGLang image pinned by `recipe.yaml`, which predates DFlash 2 and only
ships the DFlash 1 model class.

`serve.sh` validates every required overlay file, copies the image's installed
SGLang package to `/tmp/lmw-sglang-overlay/sglang`, replaces the corresponding
files from `/lmw/assets/patch/sglang`, and prepends the copy to `PYTHONPATH`.
The installed package in the image is never modified.

## Provenance and license

The overlay was imported from
`MiaAI-Lab/Qwen3.8-27B-RTX-6000-PRO-SGLang-DSpark` at commit
`c172405482871566033d4d8f5ac519e7a3ce1f79`, the same immutable source revision
recorded in `recipe.yaml`. Its Python files originate from or adapt SGLang's
DFlash 2 implementation (PRs #35371 and #35496). The tables below identify
whole-file replacements and local adaptations.

SGLang is licensed under Apache-2.0. The full upstream license is included in
`LICENSE`; each vendored Python file carries an SPDX/provenance header.

## Why this is needed

DFlash 2 (block-diffusion drafter with two-tap dynamic convolutions and a
candidate-path selector) landed in upstream SGLang only on **2026-08-19**:

- `sgl-project/sglang` PR #35371 "DFlash2: local convolution + candidate selector"
- `sgl-project/sglang` PR #35496 "Support quantized target lm_head in the DFlash2 selector"

The cookbook image predates both, so `z-lab/Qwen3.8-27B-DFlash2` (config
architecture `DFlash2DraftModel`) fails on the stock image with:

```
ValueError: Cannot find model module. 'DFlash2DraftModel' is not a registered model
in the Transformers library ... and 'AutoModel' is not present in the model config's
'auto_map' ...
```

## What is patched

Files replaced wholesale with upstream `main` (DFlash 2 feature-set only; DSPARK
workers are untouched):

| Patched file | Upstream change |
|---|---|
| `srt/models/dflash.py` | adds `DFlash2DraftModel`, `CandidateSelector`, `DFlashGroupedConv` |
| `kernels/ops/speculative/dflash.py` | adds `selector_walk_triton` |
| `srt/speculative/dflash_utils.py` | adds `is_dense_head_weight`, `table_qk_norm_rope_` |
| `srt/speculative/dflash_worker_v2.py` | DFlash 2 worker |
| `srt/speculative/dflash_info.py` | DFlash 2 verify input |
| `srt/speculative/dflash_info_v2.py` | DFlash 2 draft input |
| `srt/speculative/draft_worker_common.py` | DFlash 2 draft worker plumbing |

Files shared with other spec paths (DSPARK/EAGLE/MTP) get **appended-onto only**
(i.e. the image's file + the specific upstream function, nothing removed), so the
other algorithms keep working:

| Appended-to file | Added function |
|---|---|
| `srt/speculative/spec_utils.py` | `sample_simulated_acc_len` |
| `srt/mem_cache/allocation_sizing.py` | `page_aligned_decode_alloc_lens` |
| `srt/layers/moe/utils.py` | `draft_model_build_scope` (adapted to this image's `speculative_context` flag) |
| `srt/layers/logprob_processor.py` | `compute_spec_logprobs` |

## Updating the overlay

There is no generated patch script. To update this snapshot:

1. Review the overlay at a new immutable commit of the MiaAI-Lab source repo.
2. Copy the required files into the same relative paths under this directory.
3. Update both `metadata.source.revision` in `recipe.yaml` and the provenance
   commit above.
4. Verify that `serve.sh`'s `required` list matches the copied files, then run
   the recipe and confirm DFlash 2 initializes successfully.

Once the pinned image includes DFlash 2, remove these patch assets and the copy
overlay loop from `serve.sh`, update the image reference/digest, and launch stock
SGLang with `--speculative-algorithm DFLASH`.
