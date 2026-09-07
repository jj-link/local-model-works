package deploy

import (
	"errors"
	"fmt"
	"strings"
)

// OpenAIAPIBase returns the OpenAI-compatible API base for a deployment.
// Endpoint.Path is the recipe readiness path and is intentionally ignored.
func OpenAIAPIBase(endpoint *Endpoint) (string, error) {
	if endpoint == nil {
		return "", errors.New("deployment endpoint is nil")
	}
	host := strings.TrimSpace(endpoint.Host)
	if host == "" {
		return "", errors.New("deployment endpoint host is empty")
	}
	if endpoint.Port == 0 {
		return "", errors.New("deployment endpoint port is zero")
	}
	return fmt.Sprintf("http://%s:%d/v1", host, endpoint.Port), nil
}
