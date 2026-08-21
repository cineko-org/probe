package probe

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/gen/go/cineko/probe"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
)

func TestGeneratedProtoAPIBoundaries(t *testing.T) {
	api, err := NewHTTPAPI("https://central.example", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.request(context.Background(), "bad\nmethod", "/", "", "", "", nil, nil); err == nil {
		t.Fatal("invalid request method accepted")
	}
	invalidInput := &probepb.RegisterRequest{}
	invalidInput.SetInstallationId(string([]byte{0xff}))
	if _, err := api.newRequest(context.Background(), http.MethodPost, "/", "", "", "", invalidInput); err == nil {
		t.Fatal("invalid ProtoJSON request accepted")
	}
	if err := decodeProtoJSON([]byte(`{}`), &probepb.ClaimAssignmentResponse{}); err == nil {
		t.Fatal("invalid oneof response accepted")
	}
}

func TestGeneratedProtoCGVExecutorBoundaries(t *testing.T) {
	if err := validateScheduleTask(&observationpb.AssignmentTask{}); err == nil {
		t.Fatal("schedule task without schedule accepted")
	}
	missingTimeZone := testAssignmentTask()
	missingTimeZone.GetSchedule().SetTimeZone("")
	if err := validateScheduleTask(missingTimeZone); err == nil {
		t.Fatal("schedule task without time zone accepted")
	}
	missingDates := testAssignmentTask()
	missingDates.GetSchedule().SetTargetDates(nil)
	if err := validateScheduleTask(missingDates); err == nil {
		t.Fatal("schedule task without target dates accepted")
	}
	if scheduleTheater(nil) != (cgv.ScheduleTheater{}) {
		t.Fatal("nil theater was not empty")
	}

	executor := &CGVExecutor{clock: time.Now}
	if _, err := executor.convertCapture(cgv.ScheduleCapture{TargetDate: "invalid"}, time.UTC); err == nil {
		t.Fatal("invalid capture date accepted")
	}
	capacityOverflow := canonicalTestShowtime(cgv.ScheduleShowtime{
		Date: "2026-08-20", StartsAt: "10:00", EndsAt: "11:00", AvailableSeats: 1, Capacity: math.MaxInt32 + 1,
	})
	if _, err := executor.convertCapture(cgv.ScheduleCapture{
		TargetDate: "2026-08-20", Showtimes: []cgv.ScheduleShowtime{capacityOverflow},
	}, time.UTC); err == nil {
		t.Fatal("capacity outside int32 accepted")
	}
	availableOverflow := capacityOverflow
	availableOverflow.Capacity = 2
	availableOverflow.AvailableSeats = math.MaxInt32 + 1
	if _, err := executor.convertCapture(cgv.ScheduleCapture{
		TargetDate: "2026-08-20", Showtimes: []cgv.ScheduleShowtime{availableOverflow},
	}, time.UTC); err == nil {
		t.Fatal("available seats outside int32 accepted")
	}
	if _, err := localDate("invalid"); err == nil {
		t.Fatal("invalid local date accepted")
	}
	if _, err := int32Value(math.MaxInt32 + 1); err == nil {
		t.Fatal("out-of-range int32 value accepted")
	}
	if localDateString(nil) != "" {
		t.Fatal("nil local date formatted")
	}
}

func TestGeneratedProtoRuntimeBoundaries(t *testing.T) {
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	if err := runtime.validateMinimumPolicy(nil); err == nil {
		t.Fatal("nil heartbeat policy accepted")
	}
	if err := runtime.captureAssignment(context.Background(), nil).err; err == nil {
		t.Fatal("nil assignment accepted")
	}
	if resultOutcome(&observationpb.AssignmentResult{}) != "failed" {
		t.Fatal("result without outcome was not failed")
	}
	if assignmentTaskKind(nil) != "unknown" {
		t.Fatal("nil assignment task kind was not unknown")
	}
	catalogCapability := &observationpb.Capability{}
	catalogCapability.SetCatalogCapture(&observationpb.CatalogCapture{})
	if capabilityKey(catalogCapability) != "cgv.catalog.capture" {
		t.Fatal("catalog capability key was not canonical")
	}
	if capabilityKey(&observationpb.Capability{}) != "" {
		t.Fatal("empty capability key was not empty")
	}
	if !timestampTime(nil).IsZero() {
		t.Fatal("nil timestamp was not zero")
	}
}
