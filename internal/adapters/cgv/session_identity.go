package cgv

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	sessionIdentityFilename = "cineko-session-identity.json"
	sessionIdentityVersion  = 1
)

type persistentBrowserIdentity struct {
	Version   int                        `json:"version"`
	UserAgent browserUserAgent           `json:"userAgent"`
	Metadata  userAgentBootstrapIdentity `json:"metadata"`
	Languages []string                   `json:"languages"`
	WebGL     webGLIdentity              `json:"webgl"`
}

func loadSessionIdentity(config BrowserConfig) (*persistentBrowserIdentity, error) {
	if config.UserAgentMode != UserAgentSession {
		return nil, nil
	}
	path := filepath.Join(config.ProfileDir, sessionIdentityFilename)
	contents, err := os.ReadFile(path) // #nosec G304 -- path is inside the configured Cineko profile.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read browser session identity: %w", err)
	}
	var identity persistentBrowserIdentity
	if err := json.Unmarshal(contents, &identity); err != nil {
		return nil, fmt.Errorf("decode browser session identity: %w", err)
	}
	if err := identity.validate(); err != nil {
		return nil, fmt.Errorf("validate browser session identity: %w", err)
	}
	return &identity, nil
}

func saveSessionIdentity(config BrowserConfig, identity persistentBrowserIdentity) error {
	if config.UserAgentMode != UserAgentSession {
		return nil
	}
	identity.Version = sessionIdentityVersion
	if err := identity.validate(); err != nil {
		return fmt.Errorf("validate browser session identity: %w", err)
	}
	contents, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("encode browser session identity: %w", err)
	}
	temporary, err := os.CreateTemp(config.ProfileDir, ".session-identity-")
	if err != nil {
		return fmt.Errorf("create browser session identity: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect browser session identity: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write browser session identity: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync browser session identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close browser session identity: %w", err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(config.ProfileDir, sessionIdentityFilename)); err != nil {
		return fmt.Errorf("replace browser session identity: %w", err)
	}
	return nil
}

func (identity persistentBrowserIdentity) validate() error {
	if identity.Version != sessionIdentityVersion {
		return fmt.Errorf("unsupported version %d", identity.Version)
	}
	if err := identity.validateUserAgent(); err != nil {
		return err
	}
	if err := identity.validateMetadata(); err != nil {
		return err
	}
	return identity.validateBrowserSurfaces()
}

func (identity persistentBrowserIdentity) validateUserAgent() error {
	if strings.TrimSpace(identity.UserAgent.Value) == "" ||
		strings.TrimSpace(identity.UserAgent.Major) == "" ||
		strings.TrimSpace(identity.UserAgent.FullVersion) == "" {
		return errors.New("user agent is incomplete")
	}
	major, err := chromeMajor(identity.UserAgent.FullVersion)
	if err != nil || identity.UserAgent.Major != fmt.Sprint(major) {
		return errors.New("user agent version is inconsistent")
	}
	if !strings.Contains(identity.UserAgent.Value, "Chrome/"+identity.UserAgent.Major+".0.0.0") {
		return errors.New("user agent product version is inconsistent")
	}
	if !userAgentMatchesPlatform(identity.UserAgent.Value, identity.Metadata.Platform) {
		return errors.New("user agent platform is inconsistent")
	}
	return nil
}

func userAgentMatchesPlatform(value, platform string) bool {
	switch platform {
	case "macOS":
		return strings.Contains(value, "(Macintosh;") && !strings.Contains(value, "Windows NT") &&
			!strings.Contains(value, "X11; Linux")
	case "Windows":
		return strings.Contains(value, "(Windows NT") && !strings.Contains(value, "Macintosh;") &&
			!strings.Contains(value, "X11; Linux")
	case "Linux":
		return strings.Contains(value, "X11; Linux") && !strings.Contains(value, "Macintosh;") &&
			!strings.Contains(value, "Windows NT")
	default:
		return false
	}
}

func (identity persistentBrowserIdentity) validateMetadata() error {
	if strings.TrimSpace(identity.Metadata.Platform) == "" ||
		strings.TrimSpace(identity.Metadata.Architecture) == "" ||
		strings.TrimSpace(identity.Metadata.Bitness) == "" ||
		len(identity.Metadata.Brands) == 0 || len(identity.Metadata.FullVersionList) == 0 {
		return errors.New("user agent metadata is incomplete")
	}
	if identity.Metadata.Platform != browserUADataPlatform() || identity.Metadata.Architecture != browserArchitecture() ||
		identity.Metadata.Bitness != "64" || identity.Metadata.Mobile {
		return errors.New("user agent metadata does not match this browser runtime")
	}
	if !hasBrandVersion(identity.Metadata.Brands, "Chromium", identity.UserAgent.Major) ||
		!hasBrandVersion(identity.Metadata.FullVersionList, "Chromium", identity.UserAgent.FullVersion) {
		return errors.New("user agent brands are inconsistent")
	}
	return nil
}

func (identity persistentBrowserIdentity) validateBrowserSurfaces() error {
	if len(identity.Languages) == 0 || strings.TrimSpace(identity.Languages[0]) == "" {
		return errors.New("languages are incomplete")
	}
	if strings.TrimSpace(identity.WebGL.Vendor) == "" || strings.TrimSpace(identity.WebGL.Renderer) == "" {
		return errors.New("WebGL identity is incomplete")
	}
	if browserUADataPlatform() == "macOS" && identity.Metadata.Architecture == "arm" &&
		(!strings.Contains(identity.WebGL.Vendor, "Apple") || !strings.Contains(identity.WebGL.Renderer, "Apple")) {
		return errors.New("WebGL identity is incompatible with macOS ARM")
	}
	return nil
}

func hasBrandVersion(brands []uaBrandVersion, brand, version string) bool {
	for _, candidate := range brands {
		if candidate.Brand == brand && candidate.Version == version {
			return true
		}
	}
	return false
}
