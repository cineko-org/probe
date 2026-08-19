package contracts

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

var ErrUnsupportedProtocol = errors.New("unsupported Cineko protocol")

const (
	ProtocolVersion              = 3
	ProtocolHeader               = "X-Cineko-Protocol"
	ReleaseGenerationHeader      = "X-Cineko-Release-Generation"
	CatalogGenerationHeader      = "X-Cineko-Catalog-Generation"
	ProbeBootstrapAlgorithm      = "ES256"
	ProbeBootstrapTokenType      = "Cineko-Probe-Bootstrap" // #nosec G101 -- public JOSE type marker.
	ProbeBootstrapIssuer         = "cineko-central"
	ProbeBootstrapAudience       = "cineko-probe"
	ProbeBootstrapMaxClockSkew   = time.Minute
	ProbeBootstrapMaxConcurrent  = 1
	CapabilityCGVScheduleCapture = "cgv.schedule.capture.v2"
	CapabilityCGVCatalogCapture  = "cgv.catalog.capture.v1"
	CapabilityCGVSeatMapCapture  = "cgv.seat-map.capture.v1"
	CatalogSchemaVersion         = 1
)

var supportedCapabilities = map[string]struct{}{
	CapabilityCGVScheduleCapture: {},
	CapabilityCGVCatalogCapture:  {},
	CapabilityCGVSeatMapCapture:  {},
}

// IsSupportedCapability is the shared allowlist for Probe registration and
// bootstrap authorization. Capability names are exact, versioned wire values.
func IsSupportedCapability(capability string) bool {
	_, supported := supportedCapabilities[capability]
	return supported
}

func ProtocolHeaderValue() string {
	return strconv.Itoa(ProtocolVersion)
}

func RequireProtocol(version int) error {
	if version != ProtocolVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedProtocol, version)
	}
	return nil
}
