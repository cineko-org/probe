package probe

import (
	"context"
	"errors"
	"net/http"
	"strings"

	probepb "github.com/cineko-org/contracts/v3/gen/go/cineko/probe"
	cgvbrowser "github.com/cineko-org/probe/v2/internal/provider/cgv/browser"
)

// BrowserRuntimeConfig is shared by the standalone container and a
// Client-embedded Probe. Credentials remain supplied by the caller so a Client
// can use a short-lived bootstrap pipe without persisting it.
type BrowserRuntimeConfig struct {
	CentralURL               string
	DataDir                  string
	HTTPClient               *http.Client
	Credentials              CredentialSource
	Registration             *probepb.RegisterRequest
	SeatMapExecutor          SeatMapExecutor
	SeatAvailabilityExecutor SeatAvailabilityExecutor
	Runtime                  Config
}

// BrowserRuntime owns the exact browser factory, CGV executor, and Probe loop
// used by the container binary. Client code embeds this type instead of
// maintaining a second Probe implementation.
type BrowserRuntime struct {
	runtime *Runtime
	factory *cgvbrowser.Factory
}

func NewBrowserRuntime(config BrowserRuntimeConfig) (*BrowserRuntime, error) {
	return newBrowserRuntime(config, browserRuntimeFactories{
		factory:  cgvbrowser.NewFromEnvironment,
		executor: NewCGVExecutor,
	})
}

type browserRuntimeFactories struct {
	factory  func(string) (*cgvbrowser.Factory, error)
	executor func(*cgvbrowser.Factory) (*CGVExecutor, error)
}

func newBrowserRuntime(config BrowserRuntimeConfig, factories browserRuntimeFactories) (*BrowserRuntime, error) {
	if strings.TrimSpace(config.DataDir) == "" || config.Credentials == nil {
		return nil, errors.New("probe data directory and credential source are required")
	}
	api, err := NewHTTPAPI(config.CentralURL, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	factory, err := factories.factory(config.DataDir)
	if err != nil {
		return nil, err
	}
	executor, err := factories.executor(factory)
	if err != nil {
		factory.Close()
		return nil, err
	}
	executor.seatMap = config.SeatMapExecutor
	executor.seatAvailability = config.SeatAvailabilityExecutor
	config.Runtime.Registration = config.Registration
	runtime, err := NewRuntime(api, config.Credentials, executor, config.Runtime)
	if err != nil {
		factory.Close()
		return nil, err
	}
	return &BrowserRuntime{runtime: runtime, factory: factory}, nil
}

func (runtime *BrowserRuntime) Run(ctx context.Context) error {
	defer runtime.Close()
	return runtime.runtime.Run(ctx)
}

// Preflight verifies the local Playwright driver and Chromium process before a
// container announces readiness to its orchestrator.
func (runtime *BrowserRuntime) Preflight(ctx context.Context) error {
	if runtime == nil || runtime.factory == nil {
		return errors.New("browser runtime is not initialized")
	}
	return runtime.factory.Preflight(ctx)
}

func (runtime *BrowserRuntime) RunReady(ctx context.Context, ready chan<- error) error {
	defer runtime.Close()
	return runtime.runtime.RunReady(ctx, ready)
}

func (runtime *BrowserRuntime) SetDraining(draining bool) {
	runtime.runtime.SetDraining(draining)
}

func (runtime *BrowserRuntime) Close() {
	if runtime != nil && runtime.factory != nil {
		runtime.factory.Close()
	}
}
