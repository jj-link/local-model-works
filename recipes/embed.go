// Package recipes embeds the controller-owned templates used to compile
// supported third-party repository commits without modifying or executing them.
package recipes

import "embed"

// Templates contains the controller-managed third-party recipe bundles.
//
//go:embed qwen38-27b-rtx6000pro-dflash2 deepseek-v4-flash-0731-dspark-tp2 glm53-flash-exl3-dflash2-spark-tp2
var Templates embed.FS
