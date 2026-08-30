package recipe

import "testing"

func TestVisibleRecipeName(t *testing.T) {
	tests := []struct {
		name       string
		sourceJSON string
		fallback   string
		want       string
	}{
		{
			name:       "HTTPS GitHub remote",
			sourceJSON: `{"type":"git","remote":"https://github.com/MiaAI-Lab/GLM-5.3-Flash.git"}`,
			fallback:   "glm53-flash",
			want:       "MiaAI-Lab/GLM-5.3-Flash",
		},
		{
			name:       "SSH GitHub remote",
			sourceJSON: `{"type":"git","remote":"git@github.com:MiaAI-Lab/Qwen3.8-27B.git"}`,
			fallback:   "qwen38-27b",
			want:       "MiaAI-Lab/Qwen3.8-27B",
		},
		{
			name:       "non Git source",
			sourceJSON: `{"type":"local","remote":"/srv/recipes/qwen"}`,
			fallback:   "qwen38-27b",
			want:       "qwen38-27b",
		},
		{
			name:       "non GitHub remote",
			sourceJSON: `{"type":"git","remote":"https://git.example/MiaAI-Lab/Qwen3.8-27B.git"}`,
			fallback:   "qwen38-27b",
			want:       "qwen38-27b",
		},
		{
			name:       "malformed source",
			sourceJSON: `{`,
			fallback:   "qwen38-27b",
			want:       "qwen38-27b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := visibleRecipeName(test.sourceJSON, test.fallback); got != test.want {
				t.Fatalf("visibleRecipeName() = %q, want %q", got, test.want)
			}
		})
	}
}
