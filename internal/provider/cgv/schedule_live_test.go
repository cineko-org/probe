package cgv

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveScheduleCapture(t *testing.T) {
	if os.Getenv("CINEKO_LIVE_SCHEDULE") != "1" {
		t.Skip("set CINEKO_LIVE_SCHEDULE=1 to run the CGV schedule smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	config := liveSoxyBrowserConfig(t, ctx)
	adapter, err := NewAdapter(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	location := time.FixedZone("KST", 9*60*60)
	date := time.Now().In(location).Format(time.DateOnly)
	if configured := os.Getenv("CINEKO_LIVE_SCHEDULE_DATE"); configured != "" {
		if _, parseErr := time.ParseInLocation(time.DateOnly, configured, location); parseErr != nil {
			t.Fatalf("invalid CINEKO_LIVE_SCHEDULE_DATE: %v", parseErr)
		}
		date = configured
	}
	theater := ScheduleTheater{
		ID: CatalogID(ProviderCGV, "theater", "0013"), ProviderID: ProviderCGV,
		SourceKey: "0013", Region: "서울", Name: "용산아이파크몰",
	}
	captures, err := adapter.CaptureSchedules(ctx, theater, []string{date})
	if err != nil {
		t.Fatal(err)
	}
	if len(captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(captures))
	}
	if !captures[0].Complete {
		t.Fatalf("schedule capture incomplete: %s", captures[0].Error)
	}
	if len(captures[0].Showtimes) == 0 {
		t.Fatal("CGV schedule returned no showtimes")
	}
	t.Logf("captured %d CGV showtimes", len(captures[0].Showtimes))
}
