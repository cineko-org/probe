package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	"github.com/cineko-org/probe/v2/probe"
)

const installationIDFile = "installation-id"

var (
	version         = "0.0.0-dev"
	browserRevision = "1228"
	installationRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
)

type applicationConfig struct {
	mode         string
	centralURL   string
	dataDir      string
	registration central.RegisterProbeRequest
	credentials  probe.CredentialSource
}

func loadConfig(stdin io.Reader) (applicationConfig, error) {
	mode := strings.ToLower(envString("CINEKO_PROBE_MODE", "container"))
	if mode != "container" && mode != "client" {
		return applicationConfig{}, errors.New("CINEKO_PROBE_MODE must be container or client")
	}
	centralURL := strings.TrimSpace(os.Getenv("CINEKO_CENTRAL_URL"))
	if centralURL == "" {
		return applicationConfig{}, errors.New("CINEKO_CENTRAL_URL is required")
	}
	dataDir := envString("CINEKO_PROBE_DATA_DIR", "/var/lib/cineko-probe")
	installationID, err := resolveInstallationID(dataDir, strings.TrimSpace(os.Getenv("CINEKO_INSTALLATION_ID")))
	if err != nil {
		return applicationConfig{}, err
	}
	capabilities := []string{
		central.CapabilityCGVCatalogCapture,
		central.CapabilityCGVScheduleCapture,
	}
	if mode == "container" {
		capabilities = append(capabilities, central.CapabilityCGVSeatMapCapture)
	}
	registration := central.RegisterProbeRequest{
		InstallationID: installationID, Kind: mode,
		NetworkHint:  strings.TrimSpace(os.Getenv("CINEKO_PROBE_NETWORK_HINT")),
		Capabilities: capabilities, MaxConcurrency: 1,
		Runtime: central.Runtime{
			Version: version, Protocol: central.ProtocolVersion, BrowserRevision: browserRevision,
			Platform: runtime.GOOS, Arch: runtime.GOARCH,
		},
	}
	credentials, err := credentialSource(mode, stdin, registration)
	if err != nil {
		return applicationConfig{}, err
	}
	return applicationConfig{
		mode: mode, centralURL: centralURL, dataDir: dataDir, registration: registration, credentials: credentials,
	}, nil
}

func credentialSource(
	mode string,
	stdin io.Reader,
	registration central.RegisterProbeRequest,
) (probe.CredentialSource, error) {
	switch mode {
	case "container":
		path := strings.TrimSpace(os.Getenv("CINEKO_PROBE_ENROLLMENT_TOKEN_FILE"))
		if path == "" || !filepath.IsAbs(path) {
			return nil, errors.New("CINEKO_PROBE_ENROLLMENT_TOKEN_FILE must be an absolute Docker secret path")
		}
		credential, err := readShortSecret(path)
		if err != nil {
			return nil, err
		}
		return probe.StaticCredential(credential), nil
	case "client":
		// The Launcher owns this pipe and writes one short-lived ticket per registration.
	default:
		return nil, errors.New("probe credential mode must be container or client")
	}
	source, err := probe.NewLineCredentialSource(stdin)
	if err != nil {
		return nil, err
	}
	return probe.NewClientCredentialSource(source, probe.ClientCredentialConfig{
		PublicKeyFiles: strings.TrimSpace(os.Getenv("CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS")),
		Issuer:         envString("CINEKO_PROBE_BOOTSTRAP_ISSUER", central.ProbeBootstrapIssuer),
		Audience:       envString("CINEKO_PROBE_BOOTSTRAP_AUDIENCE", central.ProbeBootstrapAudience),
		ClockSkew:      15 * time.Second,
		Registration:   registration,
	})
}

func resolveInstallationID(dataDir, configured string) (string, error) {
	if configured != "" {
		if !installationRE.MatchString(configured) {
			return "", errors.New("CINEKO_INSTALLATION_ID has an invalid format")
		}
		return configured, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create Probe data directory: %w", err)
	}
	path := filepath.Join(dataDir, installationIDFile)
	if contents, err := os.ReadFile(path); err == nil { // #nosec G304 -- path is rooted in the configured Probe data directory.
		value := strings.TrimSpace(string(contents))
		if !installationRE.MatchString(value) {
			return "", errors.New("stored Probe installation id has an invalid format")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read Probe installation id: %w", err)
	}
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate Probe installation id: %w", err)
	}
	value := "install_" + base64.RawURLEncoding.EncodeToString(buffer)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- validated data directory.
	if errors.Is(err, os.ErrExist) {
		return resolveInstallationID(dataDir, "")
	}
	if err != nil {
		return "", fmt.Errorf("create Probe installation id: %w", err)
	}
	if _, err := file.WriteString(value + "\n"); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write Probe installation id: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync Probe installation id: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close Probe installation id: %w", err)
	}
	return value, nil
}

func readShortSecret(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304,G703 -- absolute operator-configured Docker secret path.
	if err != nil {
		return "", fmt.Errorf("open Probe enrollment token: %w", err)
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil {
		return "", fmt.Errorf("read Probe enrollment token: %w", err)
	}
	if len(contents) > 16<<10 {
		return "", errors.New("probe enrollment token exceeds size limit")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return "", errors.New("probe enrollment token is empty")
	}
	return value, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
