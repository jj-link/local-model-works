package assistant

import (
	"strings"
	"testing"
)

func TestCodexEnvironmentRemovesInheritedCredentialsAndMCP(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "parent-secret")
	t.Setenv("CODEX_API_KEY", "codex-secret")
	t.Setenv("ANTHROPIC_API_KEY", "other-secret")
	t.Setenv("MCP_SERVER_TOKEN", "mcp-secret")
	t.Setenv("CODEX_HOME", "parent-home")

	environment := codexEnvironment("isolated-home")
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{"parent-secret", "codex-secret", "other-secret", "mcp-secret", "CODEX_HOME=parent-home"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("restricted environment contains %q", forbidden)
		}
	}
	if !strings.Contains(joined, "CODEX_HOME=isolated-home") || !strings.Contains(joined, "LMW_CODEX_RESTRICTED=true") {
		t.Fatalf("restricted environment missing dedicated state markers: %s", joined)
	}
}
