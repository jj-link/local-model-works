package assistant

import (
	"context"
	"errors"
	"testing"

	"github.com/jj-link/local-model-works/internal/deploy"
)

type deploymentGetterFunc func(context.Context, string) (*deploy.Deployment, error)

func (f deploymentGetterFunc) Get(ctx context.Context, id string) (*deploy.Deployment, error) {
	return f(ctx, id)
}

func TestResolveLocalEndpointUsesOnlySelectedHealthyDeployment(t *testing.T) {
	requested := "selected"
	getter := deploymentGetterFunc(func(_ context.Context, id string) (*deploy.Deployment, error) {
		if id != requested {
			t.Fatalf("lookup id = %q", id)
		}
		return &deploy.Deployment{
			ID: id, DesiredState: "running", ObservedState: "healthy",
			Endpoint: &deploy.Endpoint{Host: "127.0.0.1", Port: 8000, Model: "served-model"},
		}, nil
	})
	endpoint, err := ResolveLocalEndpoint(context.Background(), getter, requested)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.DeploymentID != requested || endpoint.URL != "http://127.0.0.1:8000/v1" || endpoint.Model != "served-model" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
}

func TestResolveLocalEndpointRejectsUnhealthyAndUnknown(t *testing.T) {
	unhealthy := deploymentGetterFunc(func(context.Context, string) (*deploy.Deployment, error) {
		return &deploy.Deployment{ID: "selected", DesiredState: "running", ObservedState: "failed"}, nil
	})
	if code := assistantErrorCode(ResolveLocalEndpoint(context.Background(), unhealthy, "selected")); code != "assistant.deployment_not_ready" {
		t.Fatalf("unhealthy code = %q", code)
	}
	unknown := deploymentGetterFunc(func(context.Context, string) (*deploy.Deployment, error) { return nil, deploy.ErrUnknown })
	if code := assistantErrorCode(ResolveLocalEndpoint(context.Background(), unknown, "selected")); code != "assistant.deployment_unknown" {
		t.Fatalf("unknown code = %q", code)
	}
}

func assistantErrorCode(_ LocalEndpoint, err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func TestLocalTransportDoesNotTrustConfiguredHTTP(t *testing.T) {
	const address = "http://192.168.1.42:8000/v1"
	if err := ValidateEndpoint(address); err == nil {
		t.Fatal("external provider accepted non-loopback HTTP")
	}
	if _, err := LocalProvider(LocalEndpoint{URL: address, Model: "model"}); err == nil {
		t.Fatal("caller forged a trusted local endpoint")
	}
	endpoint, err := ResolveLocalEndpoint(context.Background(), deploymentGetterFunc(func(context.Context, string) (*deploy.Deployment, error) {
		return &deploy.Deployment{ID: "chosen", DesiredState: "running", ObservedState: "healthy", Endpoint: &deploy.Endpoint{Host: "192.168.1.42", Port: 8000, Model: "model"}}, nil
	}), "chosen")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := LocalProvider(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := provider.completionURL(); err != nil || got != address+"/chat/completions" {
		t.Fatalf("resolved local endpoint rejected: %s %v", got, err)
	}
	endpoint.URL = "http://192.168.1.99/v1"
	if _, err := LocalProvider(endpoint); err == nil {
		t.Fatal("mutated local endpoint retained trust")
	}
	provider.BaseURL = "http://192.168.1.99/v1"
	if _, err := provider.completionURL(); err == nil {
		t.Fatal("mutated provider retained local trust")
	}
}
