package assistant

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/deploy"
)

type DeploymentGetter interface {
	Get(context.Context, string) (*deploy.Deployment, error)
}

type LocalEndpoint struct {
	DeploymentID string
	URL          string
	Model        string
	resolvedURL  string
}

// ResolveLocalEndpoint resolves only the explicitly configured deployment. It
// never lists deployments, substitutes another model, or starts a workload.
func ResolveLocalEndpoint(ctx context.Context, deployments DeploymentGetter, deploymentID string) (LocalEndpoint, error) {
	if strings.TrimSpace(deploymentID) == "" {
		return LocalEndpoint{}, &Error{Code: "assistant.deployment_required", Message: "local provider requires a deployment", Retryable: false}
	}
	deployment, err := deployments.Get(ctx, deploymentID)
	if err != nil {
		if errors.Is(err, deploy.ErrUnknown) {
			return LocalEndpoint{}, &Error{Code: "assistant.deployment_unknown", Message: "selected deployment does not exist", Retryable: false}
		}
		return LocalEndpoint{}, &Error{Code: "assistant.deployment_unavailable", Message: "selected deployment could not be read", Retryable: true}
	}
	if deployment.ID != deploymentID {
		return LocalEndpoint{}, &Error{Code: "assistant.deployment_mismatch", Message: "deployment lookup returned a different deployment", Retryable: false}
	}
	if deployment.DesiredState != "running" || deployment.ObservedState != "healthy" {
		return LocalEndpoint{}, &Error{Code: "assistant.deployment_not_ready", Message: "selected deployment must be running and healthy", Retryable: true}
	}
	endpoint := deployment.Endpoint
	if endpoint == nil || strings.TrimSpace(endpoint.Host) == "" || endpoint.Port <= 0 || strings.TrimSpace(endpoint.Model) == "" {
		return LocalEndpoint{}, &Error{Code: "assistant.endpoint_unavailable", Message: "selected deployment has no complete model endpoint", Retryable: true}
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(endpoint.Host, strconv.Itoa(int(endpoint.Port))), Path: "/v1"}
	return LocalEndpoint{DeploymentID: deploymentID, URL: u.String(), Model: endpoint.Model, resolvedURL: u.String()}, nil
}

// LocalProvider accepts only an endpoint produced by the deployment resolver.
func LocalProvider(endpoint LocalEndpoint) (*OpenAICompatible, error) {
	if endpoint.resolvedURL == "" || endpoint.URL != endpoint.resolvedURL {
		return nil, invalid("assistant.endpoint_unavailable", "local endpoint must be resolved from a healthy deployment")
	}
	return &OpenAICompatible{BaseURL: endpoint.URL, Model: endpoint.Model, trustedLocalURL: endpoint.URL}, nil
}
