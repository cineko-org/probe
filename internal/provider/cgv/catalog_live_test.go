package cgv

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveCatalogCapture(t *testing.T) {
	if os.Getenv("CINEKO_LIVE_CATALOG") != "1" {
		t.Skip("set CINEKO_LIVE_CATALOG=1 to run the CGV catalog smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	config := DefaultBrowserConfig()
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = t.TempDir()
	adapter, err := NewAdapter(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	catalog, err := adapter.CaptureCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Theaters) == 0 {
		t.Fatal("CGV catalog returned no theaters")
	}
	for _, theater := range catalog.Theaters {
		if !providerSiteIdentifier(theater.SourceKey) {
			t.Errorf("CGV theater %q has unsupported site number %q", theater.Name, theater.SourceKey)
		}
	}
	t.Logf("captured %d CGV theaters", len(catalog.Theaters))
}
