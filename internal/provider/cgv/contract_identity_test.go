package cgv

import (
	"strings"
	"testing"
)

func TestProviderSiteIdentifierIsOpaqueAndBounded(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"0056", "P001", "P004", "P013"} {
		if !providerSiteIdentifier(value) {
			t.Fatalf("valid provider site identifier %q rejected", value)
		}
	}
	for _, value := range []string{"", strings.Repeat("x", 65)} {
		if providerSiteIdentifier(value) {
			t.Fatalf("invalid provider site identifier of length %d accepted", len(value))
		}
	}
}
