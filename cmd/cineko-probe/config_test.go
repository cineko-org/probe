package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/gen/go/cineko/probe"
	"github.com/cineko-org/probe/v2/internal/bootstrap"
	"google.golang.org/protobuf/proto"
)

func TestLoadContainerConfig(t *testing.T) {
	clearProbeEnvironment(t)
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "enrollment-token")
	if err := os.WriteFile(secretPath, []byte(" container-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CINEKO_CENTRAL_URL", "https://central.cineko.invalid")
	t.Setenv("CINEKO_PROBE_DATA_DIR", directory)
	t.Setenv("CINEKO_PROBE_ENROLLMENT_TOKEN_FILE", secretPath)
	t.Setenv("CINEKO_INSTALLATION_ID", "install_container_01")
	t.Setenv("CINEKO_PROBE_NETWORK_HINT", " home-seoul ")

	config, err := loadConfig(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if config.mode != "container" || config.centralURL != "https://central.cineko.invalid" || config.dataDir != directory ||
		config.registration.GetInstallationId() != "install_container_01" ||
		config.registration.GetKind().GetContainer() == nil || config.registration.GetNetworkHint() != "home-seoul" ||
		len(config.registration.GetCapabilities()) != 3 || config.registration.GetCapabilities()[0].GetCatalogCapture() == nil ||
		config.registration.GetCapabilities()[1].GetScheduleCapture() == nil || config.registration.GetCapabilities()[2].GetSeatMapCapture() == nil ||
		config.registration.GetRuntime().GetPlatform() != runtime.GOOS || config.registration.GetRuntime().GetArchitecture() != runtime.GOARCH {
		t.Fatalf("container config = %+v", config)
	}
	credential, err := config.credentials.Credential(context.Background())
	if err != nil || credential != "container-token" {
		t.Fatalf("container credential = %q, %v", credential, err)
	}
}

func TestLoadClientConfigAndVerifyBootstrap(t *testing.T) {
	clearProbeEnvironment(t)
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(t.TempDir(), "bootstrap-public.pem")
	if err := os.WriteFile(
		publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	registration := &probepb.RegisterRequest{}
	registration.SetInstallationId("install_client_01")
	kind := &probepb.ProbeKind{}
	kind.SetClient(&probepb.ClientProbe{})
	registration.SetKind(kind)
	registration.SetCapabilities([]*observationpb.Capability{capabilityCatalogCapture(), capabilityScheduleCapture()})
	registration.SetMaxConcurrency(1)
	runtimeInfo := &commonpb.Runtime{}
	runtimeInfo.SetComponentVersion(version)
	runtimeInfo.SetBrowserRevision(browserRevision)
	runtimeInfo.SetPlatform(runtime.GOOS)
	runtimeInfo.SetArchitecture(runtime.GOARCH)
	registration.SetRuntime(runtimeInfo)
	signer, err := bootstrap.NewSigner("cineko-central", "cineko-probe", "current", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := signer.Issue(bootstrap.Claims{
		UserID: "user_01", TicketID: "ticket_01", InstallationID: registration.GetInstallationId(),
		DeviceID: "device_01", Kind: "client", Capabilities: []string{"cgv.catalog.capture", "cgv.schedule.capture"},
		MaxConcurrency: 1, RuntimeVersion: version, BrowserRevision: browserRevision,
		Platform: runtime.GOOS, Architecture: runtime.GOARCH,
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CINEKO_PROBE_MODE", "client")
	t.Setenv("CINEKO_CENTRAL_URL", "https://central.cineko.invalid")
	t.Setenv("CINEKO_PROBE_DATA_DIR", t.TempDir())
	t.Setenv("CINEKO_INSTALLATION_ID", registration.GetInstallationId())
	t.Setenv("CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS", "current="+publicPath)

	config, err := loadConfig(strings.NewReader(ticket + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := config.credentials.Credential(context.Background())
	if err != nil || credential != ticket || !proto.Equal(config.registration, registration) {
		t.Fatalf("client credential/config = %q, %v, %+v", credential, err, config.registration)
	}
}

func TestLoadConfigRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T)
	}{
		{name: "mode", set: func(t *testing.T) { t.Setenv("CINEKO_PROBE_MODE", "invalid") }},
		{name: "central URL", set: func(t *testing.T) { t.Setenv("CINEKO_CENTRAL_URL", "") }},
		{name: "container token path", set: func(t *testing.T) {
			t.Setenv("CINEKO_CENTRAL_URL", "https://central.invalid")
			t.Setenv("CINEKO_PROBE_ENROLLMENT_TOKEN_FILE", "relative-secret")
		}},
		{name: "client keyring", set: func(t *testing.T) {
			t.Setenv("CINEKO_PROBE_MODE", "client")
			t.Setenv("CINEKO_CENTRAL_URL", "https://central.invalid")
			t.Setenv("CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS", "")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearProbeEnvironment(t)
			t.Setenv("CINEKO_PROBE_DATA_DIR", t.TempDir())
			t.Setenv("CINEKO_INSTALLATION_ID", "install_test_01")
			test.set(t)
			if _, err := loadConfig(strings.NewReader("")); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestResolveInstallationIDLifecycle(t *testing.T) {
	directory := t.TempDir()
	if _, err := resolveInstallationID(directory, "bad"); err == nil {
		t.Fatal("invalid configured installation id accepted")
	}
	first, err := resolveInstallationID(directory, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveInstallationID(directory, "")
	if err != nil || second != first {
		t.Fatalf("persisted installation id = %q, %v; first = %q", second, err, first)
	}
	info, err := os.Stat(filepath.Join(directory, installationIDFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("installation id permissions = %v, %v", info.Mode().Perm(), err)
	}
	invalidDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidDirectory, installationIDFile), []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInstallationID(invalidDirectory, ""); err == nil {
		t.Fatal("invalid stored installation id accepted")
	}
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInstallationID(filePath, ""); err == nil {
		t.Fatal("file data directory accepted")
	}
}

func TestReadShortSecretBoundaries(t *testing.T) {
	directory := t.TempDir()
	for _, test := range []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "value", contents: " token\n", want: "token"},
		{name: "empty", contents: " \n", wantErr: true},
		{name: "oversized", contents: strings.Repeat("x", (16<<10)+1), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, test.name)
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			value, err := readShortSecret(path)
			if (err != nil) != test.wantErr || value != test.want {
				t.Fatalf("secret = %q, %v", value, err)
			}
		})
	}
	if _, err := readShortSecret(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing secret accepted")
	}
}

func TestCredentialSourceRejectsMissingReader(t *testing.T) {
	clearProbeEnvironment(t)
	if _, err := credentialSource("client", nil, &probepb.RegisterRequest{}); err == nil {
		t.Fatal("nil Client credential pipe accepted")
	}
	if _, err := credentialSource("invalid", strings.NewReader(""), &probepb.RegisterRequest{}); err == nil {
		t.Fatal("unknown credential mode accepted")
	}
}

func clearProbeEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CINEKO_PROBE_MODE", "CINEKO_CENTRAL_URL", "CINEKO_PROBE_DATA_DIR", "CINEKO_INSTALLATION_ID",
		"CINEKO_PROBE_NETWORK_HINT", "CINEKO_PROBE_ENROLLMENT_TOKEN_FILE",
		"CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS", "CINEKO_PROBE_BOOTSTRAP_ISSUER",
		"CINEKO_PROBE_BOOTSTRAP_AUDIENCE",
	} {
		t.Setenv(name, "")
	}
}
