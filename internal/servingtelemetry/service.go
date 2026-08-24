package servingtelemetry

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/telemetry"
)

// PollInterval is how often the control plane samples serving endpoints.
const PollInterval = 5 * time.Second

// MaxConcurrentProbes bounds the per-collect probe fan-out.
const MaxConcurrentProbes = 8

// DeploymentSource lists deployments with their resolved endpoint metadata.
// *deploy.Service satisfies this, so the poller needs no recipe re-parsing.
type DeploymentSource interface {
	List(context.Context) ([]deploy.Deployment, error)
}

// Service runs the five-second serving telemetry poller and persists typed
// samples through the telemetry store. Monitoring never mutates deployment
// observed state.
type Service struct {
	source DeploymentSource
	tel    *telemetry.Service
	prober *Prober
}

// New builds the poller. An optional trailing *http.Client overrides the
// production client (two-second timeout, redirects disabled) for tests.
func New(source DeploymentSource, svc *telemetry.Service, client ...*http.Client) *Service {
	httpClient := defaultClient()
	if len(client) > 0 && client[0] != nil {
		httpClient = client[0]
	}
	return &Service{
		source: source,
		tel:    svc,
		prober: NewProber(httpClient),
	}
}

func defaultClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse // redirects disabled: GET-only probes
		},
	}
}

// Run collects immediately and then on every poll interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	_ = s.Collect(ctx, time.Now())
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Collect(ctx, time.Now())
		}
	}
}

// Collect probes every eligible deployment and persists the resulting samples.
// Only desired-running deployments in healthy/degraded state with a recorded
// endpoint are probed; concurrent probes are capped.
func (s *Service) Collect(ctx context.Context, now time.Time) error {
	deps, err := s.source.List(ctx)
	if err != nil {
		return err
	}
	var targets []deploy.Deployment
	for _, d := range deps {
		if eligible(d) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	workers := len(targets)
	if workers > MaxConcurrentProbes {
		workers = MaxConcurrentProbes
	}
	jobc := make(chan deploy.Deployment)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var probeErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dep := range jobc {
				payload := s.prober.Probe(ctx, dep)
				s.annotateModel(&payload, dep)
				if err := s.tel.IngestServing(ctx, dep.ID, now.Unix(), payload); err != nil {
					mu.Lock()
					if probeErr == nil {
						probeErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, d := range targets {
		jobc <- d
	}
	close(jobc)
	wg.Wait()
	return probeErr
}

// eligible selects deployments worth probing.
func eligible(d deploy.Deployment) bool {
	if d.DesiredState != "running" {
		return false
	}
	if d.ObservedState != "healthy" && d.ObservedState != "degraded" {
		return false
	}
	if d.Endpoint == nil || d.Endpoint.Host == "" || d.Endpoint.Port == 0 {
		return false
	}
	return true
}

// annotateModel prefers the persisted endpoint model identity (survives
// restart) over whatever the probe derived.
func (s *Service) annotateModel(p *telemetry.ServingPayload, dep deploy.Deployment) {
	if dep.Endpoint != nil && dep.Endpoint.Model != "" {
		p.ModelID = dep.Endpoint.Model
	}
}
