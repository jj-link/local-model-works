// Package recipes embeds reviewed execution metadata for upstream-authored
// procedures. Source repositories and their lifecycle scripts are not rebuilt.
package recipes

import "embed"

// Templates contains metadata only; execution uses the exact original checkout.
//
//go:embed qwen38-27b-rtx6000pro-dflash2/recipe.yaml deepseek-v4-flash-vision-exp-dspark-tp2/recipe.yaml glm53-flash-exl3-dflash2-spark-tp2/recipe.yaml qwen38-27b-dgx-spark-mtp/recipe.yaml qwen38-flash-next-dspark-tp2/recipe.yaml qwen38-flash-next-spark-tp1/recipe.yaml
var Templates embed.FS
