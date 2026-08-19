package probe

import (
	"context"
	"errors"
	"testing"

	contracts "github.com/cineko-org/contracts/v3"
	"github.com/cineko-org/probe/v2/internal/adapters/browserfactory"
)

func TestNewBrowserRuntimeValidatesPublicConfiguration(t *testing.T) {
	if _, err := NewBrowserRuntime(BrowserRuntimeConfig{}); err == nil {
		t.Fatal("empty browser runtime configuration accepted")
	}
	config := BrowserRuntimeConfig{DataDir: t.TempDir(), Credentials: StaticCredential("token")}
	if _, err := NewBrowserRuntime(config); err == nil {
		t.Fatal("empty Central URL accepted")
	}
	config.CentralURL = "https://central.example.test"
	config.Registration = contracts.RegisterProbeRequest{Kind: "invalid", MaxConcurrency: 1}
	if _, err := NewBrowserRuntime(config); err == nil {
		t.Fatal("invalid registration accepted")
	}
	t.Setenv("CINEKO_SOXY_URL", "https://soxy.example.test")
	t.Setenv("CINEKO_SOXY_API_TOKEN", "")
	config.Registration = contracts.RegisterProbeRequest{Kind: "client", MaxConcurrency: 1}
	if _, err := NewBrowserRuntime(config); err == nil {
		t.Fatal("invalid egress configuration accepted")
	}
}

func TestNewBrowserRuntimeClosesFactoryOnExecutorFailure(t *testing.T) {
	t.Setenv("CINEKO_SOXY_URL", "")
	t.Setenv("CINEKO_SOXY_API_TOKEN", "")
	config := BrowserRuntimeConfig{
		CentralURL: "https://central.example.test", DataDir: t.TempDir(),
		Credentials:  StaticCredential("token"),
		Registration: contracts.RegisterProbeRequest{Kind: "client", MaxConcurrency: 1},
	}
	factory, err := browserfactory.NewFromEnvironment(config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newBrowserRuntime(config, browserRuntimeFactories{
		factory:  func(string) (*browserfactory.Factory, error) { return factory, nil },
		executor: func(*browserfactory.Factory) (*CGVExecutor, error) { return nil, errors.New("executor") },
	})
	if err == nil {
		t.Fatal("executor failure ignored")
	}
	if _, err := factory.Open(context.Background(), browserfactory.Task{}); !errors.Is(err, browserfactory.ErrClosed) {
		t.Fatalf("factory was not closed: %v", err)
	}
}

func TestBrowserRuntimeNilClose(t *testing.T) {
	t.Parallel()
	var runtime *BrowserRuntime
	if err := runtime.Preflight(context.Background()); err == nil {
		t.Fatal("nil browser runtime preflight accepted")
	}
	runtime.Close()
}

func TestBrowserRuntimePublicLifecycle(t *testing.T) {
	t.Setenv("CINEKO_SOXY_URL", "")
	t.Setenv("CINEKO_SOXY_API_TOKEN", "")
	runtime, err := NewBrowserRuntime(BrowserRuntimeConfig{
		CentralURL:  "https://central.example.test",
		DataDir:     t.TempDir(),
		Credentials: StaticCredential("token"),
		Registration: contracts.RegisterProbeRequest{
			InstallationID: "install_test", Kind: "client", MaxConcurrency: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	runtime.SetDraining(true)
	if err := runtime.RunReady(context.Background(), nil); err == nil {
		t.Fatal("nil readiness channel accepted")
	}
	runtime.Close()
}

func TestBrowserRuntimeRunRejectsNilContext(t *testing.T) {
	t.Parallel()
	probeRuntime, err := NewRuntime(
		&fakeAPI{}, StaticCredential("token"), &fakeExecutor{err: errors.New("unused")},
		Config{Registration: contracts.RegisterProbeRequest{Kind: "container", MaxConcurrency: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	browser := &BrowserRuntime{runtime: probeRuntime}
	if err := browser.Run(nil); err == nil { //nolint:staticcheck // verifies the public nil-context guard.
		t.Fatal("nil context accepted")
	}
	browser.SetDraining(true)
}
