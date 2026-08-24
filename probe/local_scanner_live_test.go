package probe

import (
	"context"
	"os"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
)

func TestLiveLocalScannerCaptureScheduleWeekdays(t *testing.T) {
	if os.Getenv("CINEKO_LIVE_SCHEDULE_WEEKDAYS") != "1" {
		t.Skip("set CINEKO_LIVE_SCHEDULE_WEEKDAYS=1 to run the filtered local scanner smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	scanner, err := NewLocalScanner(LocalScannerConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := scanner.Close(); closeErr != nil {
			t.Errorf("close local scanner: %v", closeErr)
		}
	}()

	sourceKey := "0013"
	theater := &catalogpb.Theater{}
	theater.SetId(cgv.CatalogID(cgv.ProviderCGV, "theater", sourceKey))
	theater.SetProviderId(cgv.ProviderCGV)
	theater.SetIdentity(cgv.NewTheaterIdentity(sourceKey))
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	weekdays := []int32{
		int32(time.Thursday), int32(time.Friday),
	}
	var durations [2]time.Duration
	for round := range 2 {
		startedAt := time.Now()
		captures, captureErr := scanner.CaptureScheduleWeekdays(ctx, theater, weekdays)
		durations[round] = time.Since(startedAt)
		if captureErr != nil {
			t.Fatalf("round %d: %v", round+1, captureErr)
		}
		for _, capture := range captures {
			if !capture.GetComplete() {
				t.Fatalf("round %d returned incomplete date %+v: %s", round+1, capture.GetTargetDate(), capture.GetErrorCode())
			}
			targetDate := capture.GetTargetDate()
			date := time.Date(int(targetDate.GetYear()), time.Month(targetDate.GetMonth()), int(targetDate.GetDay()), 0, 0, 0, 0, time.UTC)
			if date.Weekday() != time.Thursday && date.Weekday() != time.Friday {
				t.Fatalf("unexpected provider weekday %s in %+v", date.Weekday(), targetDate)
			}
		}
		t.Logf("round %d captured %d filtered provider dates in %s", round+1, len(captures), durations[round])
	}
	t.Logf("cold=%s warm=%s improvement=%.1f%%", durations[0], durations[1], 100*(1-float64(durations[1])/float64(durations[0])))
}
