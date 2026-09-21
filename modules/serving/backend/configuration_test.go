package backend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeploymentConfigurationRejectsUnreviewedOrOutOfScopeRequests(t *testing.T) {
	module := &Module{}
	for _, test := range []struct {
		name  string
		apply bool
		body  string
	}{
		{name: "version switch", body: `{"target_digest":"sha256:other","parameters":{}}`},
		{name: "placement change", body: `{"placements":[{"node_id":"other","rank":0}]}`},
		{name: "fabric change", body: `{"fabric":"other"}`},
		{name: "missing review", apply: true, body: `{"parameters":{"context_length":8192}}`},
		{name: "unreviewed version switch", apply: true, body: `{"target_digest":"sha256:other","plan_digest":"reviewed"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/deployments/dep/configuration", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			if test.apply {
				module.ApplyDeploymentConfiguration(response, request, "dep")
			} else {
				module.PlanDeploymentConfiguration(response, request, "dep")
			}
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("out-of-scope configuration accepted: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
