package cgv

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	"github.com/cineko-org/probe/v2/internal/egress"
)

func liveSoxyBrowserConfig(t *testing.T, ctx context.Context) BrowserConfig {
	t.Helper()
	manager, err := egress.NewFromEnvironment()
	if err != nil {
		t.Fatalf("configure live Soxy egress: %v", err)
	}
	lease, err := manager.Acquire(ctx, egress.PurposeScan)
	if err != nil {
		t.Fatalf("acquire live Soxy scan lease: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Errorf("release live Soxy scan lease: %v", closeErr)
		}
	})
	proxy := lease.Proxy()
	if proxy == nil {
		t.Fatal("live CGV smoke requires a Soxy proxy lease")
	}
	config := DefaultBrowserConfig()
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = t.TempDir()
	config.Proxy = &BrowserProxy{
		Server: proxy.Server, Username: proxy.Username, Password: proxy.Password,
	}
	return config
}

func TestLiveSeatMapCapture(t *testing.T) {
	if os.Getenv("CINEKO_LIVE_SEAT_MAP") != "1" {
		t.Skip("set CINEKO_LIVE_SEAT_MAP=1 to run the CGV seat-map smoke test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	config := liveSoxyBrowserConfig(t, ctx)
	adapter, err := NewAdapter(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	location := time.FixedZone("KST", 9*60*60)
	date := time.Now().In(location).Format(time.DateOnly)
	theater := ScheduleTheater{
		ID: CatalogID(ProviderCGV, "theater", "0013"), ProviderID: ProviderCGV,
		SourceKey: "0013", Region: "서울", Name: "용산아이파크몰",
	}
	captures, err := adapter.CaptureSchedules(ctx, theater, []string{date})
	if err != nil {
		t.Fatal(err)
	}
	if len(captures) != 1 || !captures[0].Complete || len(captures[0].Showtimes) == 0 {
		t.Fatalf("current schedule is not usable for seat-map smoke: %+v", captures)
	}
	defer adapter.Close()
	showtime := captures[0].Showtimes[0]
	identityParts := strings.Split(showtime.AuditoriumSourceKey, "/")
	if len(identityParts) != 2 || identityParts[0] != theater.SourceKey || identityParts[1] == "" {
		t.Fatalf("unexpected auditorium source identity %q", showtime.AuditoriumSourceKey)
	}
	theaterMessage := &catalogpb.Theater{}
	theaterMessage.SetId(theater.ID)
	theaterMessage.SetProviderId(theater.ProviderID)
	theaterMessage.SetIdentity(NewTheaterIdentity(theater.SourceKey))
	theaterMessage.SetRegion(theater.Region)
	theaterMessage.SetName(theater.Name)
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(showtime.AuditoriumID)
	auditorium.SetTheaterId(theater.ID)
	auditorium.SetIdentity(NewAuditoriumIdentity(identityParts[0], identityParts[1]))
	auditorium.SetName(showtime.AuditoriumName)
	seatMapTask := &observationpb.SeatMapTask{}
	seatMapTask.SetTheater(theaterMessage)
	seatMapTask.SetAuditorium(auditorium)
	seatMapTask.SetLocale("ko-KR")
	seatMapTask.SetTimeZone("Asia/Seoul")
	task := &observationpb.AssignmentTask{}
	task.SetSeatMap(seatMapTask)

	snapshot, err := adapter.CaptureSeatMap(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetAuditoriumId() != auditorium.GetId() || len(snapshot.GetLayout().GetSeats()) == 0 {
		t.Fatalf("captured invalid auditorium layout: %+v", snapshot)
	}
	t.Logf("captured auditorium=%q seats=%d", auditorium.GetName(), len(snapshot.GetLayout().GetSeats()))
}
