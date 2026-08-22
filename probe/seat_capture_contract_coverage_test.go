package probe

import (
	"errors"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/v3/gen/go/cineko/seatmap"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCanonicalShowtimeContractBoundaries(t *testing.T) {
	t.Parallel()
	valid := func() cgv.ScheduleShowtime {
		return canonicalTestShowtime(cgv.ScheduleShowtime{
			Date: "2026-08-12", StartsAt: "10:00", EndsAt: "11:00",
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*cgv.ScheduleShowtime)
	}{
		{name: "provider", mutate: func(value *cgv.ScheduleShowtime) { value.ProviderID = "" }},
		{name: "theater missing", mutate: func(value *cgv.ScheduleShowtime) { value.TheaterID = "" }},
		{name: "tuple length", mutate: func(value *cgv.ScheduleShowtime) { value.SourceKey = "0056/2026-08-12/0007" }},
		{name: "site number", mutate: func(value *cgv.ScheduleShowtime) { value.SourceKey = "site/2026-08-12/0007/0003" }},
		{name: "screen number", mutate: func(value *cgv.ScheduleShowtime) { value.SourceKey = "0056/2026-08-12/screen/0003" }},
		{name: "sequence", mutate: func(value *cgv.ScheduleShowtime) { value.SourceKey = "0056/2026-08-12/0007/sequence" }},
		{name: "date", mutate: func(value *cgv.ScheduleShowtime) { value.SourceKey = "0056/2026-02-30/0007/0003" }},
		{name: "auditorium tuple", mutate: func(value *cgv.ScheduleShowtime) { value.AuditoriumSourceKey = "0056" }},
		{name: "showtime id", mutate: func(value *cgv.ScheduleShowtime) { value.ID = "wrong" }},
		{name: "movie source", mutate: func(value *cgv.ScheduleShowtime) { value.MovieSourceKey = "" }},
		{name: "movie id", mutate: func(value *cgv.ScheduleShowtime) { value.MovieID = "wrong" }},
		{name: "auditorium id", mutate: func(value *cgv.ScheduleShowtime) { value.AuditoriumID = "wrong" }},
		{name: "theater id", mutate: func(value *cgv.ScheduleShowtime) { value.TheaterID = "wrong" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := valid()
			test.mutate(&value)
			if _, _, err := canonicalShowtimeContract(value); !errors.Is(err, cgv.ErrIdentityMismatch) {
				t.Fatalf("canonicalShowtimeContract() error = %v", err)
			}
		})
	}
	parts, identity, err := canonicalShowtimeContract(valid())
	if err != nil || len(parts) != 4 || identity == nil {
		t.Fatalf("canonical showtime = %v, %+v, %v", parts, identity, err)
	}
	for _, test := range []struct {
		input string
		want  bool
	}{
		{input: ""},
		{input: "12a"},
		{input: " 0056 ", want: true},
	} {
		if got := numericIdentifier(test.input); got != test.want {
			t.Fatalf("numericIdentifier(%q) = %t, want %t", test.input, got, test.want)
		}
	}
}

func TestExecutionOutputContractBoundaries(t *testing.T) {
	t.Parallel()
	validLiveSeat := validLiveSeatObservation("showtime", "auditorium")
	validCatalog := catalogSnapshot(time.Now().UTC())
	invalidCatalog := catalogSnapshot(time.Now().UTC())
	invalidCatalog.SetObservedAt(&timestamppb.Timestamp{Seconds: 253402300800})
	for _, test := range []struct {
		name   string
		output executionOutput
		valid  bool
	}{
		{name: "missing", output: executionOutput{}},
		{name: "multiple", output: executionOutput{liveSeat: validLiveSeat, catalog: validCatalog}},
		{name: "invalid live seat", output: executionOutput{liveSeat: &seatmappb.LiveSeatObservation{}}},
		{name: "valid live seat", output: executionOutput{liveSeat: validLiveSeat}, valid: true},
		{name: "missing catalog metadata", output: executionOutput{catalog: &catalogpb.CatalogSnapshot{}}},
		{name: "invalid catalog timestamp", output: executionOutput{catalog: invalidCatalog}},
		{name: "valid catalog", output: executionOutput{catalog: validCatalog}, valid: true},
		{name: "empty captures", output: executionOutput{captures: []*observationpb.Capture{}}},
		{name: "valid captures", output: executionOutput{captures: []*observationpb.Capture{capture(true)}}, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateExecutionOutput(test.output)
			if test.valid && err != nil {
				t.Fatalf("validateExecutionOutput() error = %v", err)
			}
			if !test.valid && !errors.Is(err, errInvalidExecutionOutput) {
				t.Fatalf("validateExecutionOutput() error = %v", err)
			}
		})
	}
}

func TestLiveSeatAssignmentIdentityBoundaries(t *testing.T) {
	t.Parallel()
	seatMapTask := seatMapAssignmentTask()
	seatMapAuditorium := seatMapTask.GetSeatMap().GetAuditorium().GetId()
	seatMapShowtime := seatMapTask.GetSeatMap().GetShowtime().GetId()
	if auditoriumID, showtimeID, err := liveSeatAssignmentIDs(seatMapTask); err != nil ||
		auditoriumID != seatMapAuditorium || showtimeID != seatMapShowtime {
		t.Fatalf("seat-map assignment IDs = %q, %q, %v", auditoriumID, showtimeID, err)
	}
	withoutShowtime := seatMapAssignmentTask()
	withoutShowtime.GetSeatMap().ClearShowtime()
	if _, showtimeID, err := liveSeatAssignmentIDs(withoutShowtime); err != nil || showtimeID != "" {
		t.Fatalf("seat-map assignment without showtime = %q, %v", showtimeID, err)
	}
	missingAuditorium := seatMapAssignmentTask()
	missingAuditorium.GetSeatMap().ClearAuditorium()
	if _, _, err := liveSeatAssignmentIDs(missingAuditorium); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("missing seat-map auditorium error = %v", err)
	}

	availabilityTask := seatAvailabilityAssignmentTaskForProbe()
	availabilityAuditorium := availabilityTask.GetSeatAvailability().GetAuditorium().GetId()
	availabilityShowtime := availabilityTask.GetSeatAvailability().GetShowtime().GetId()
	if auditoriumID, showtimeID, err := liveSeatAssignmentIDs(availabilityTask); err != nil ||
		auditoriumID != availabilityAuditorium || showtimeID != availabilityShowtime {
		t.Fatalf("availability assignment IDs = %q, %q, %v", auditoriumID, showtimeID, err)
	}
	missingAvailabilityTarget := seatAvailabilityAssignmentTaskForProbe()
	missingAvailabilityTarget.GetSeatAvailability().ClearAuditorium()
	if _, _, err := liveSeatAssignmentIDs(missingAvailabilityTarget); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("missing availability target error = %v", err)
	}
	if _, _, err := liveSeatAssignmentIDs(nil); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("missing live-seat assignment error = %v", err)
	}

	valid := validLiveSeatObservation(seatMapShowtime, seatMapAuditorium)
	if err := validateLiveSeatCapture(seatMapTask, valid); err != nil {
		t.Fatalf("valid seat-map capture rejected: %v", err)
	}
	withoutShowtimeLiveSeat := validLiveSeatObservation("any-showtime", seatMapAuditorium)
	if err := validateLiveSeatCapture(withoutShowtime, withoutShowtimeLiveSeat); err != nil {
		t.Fatalf("showtime-independent seat-map capture rejected: %v", err)
	}
	mismatchedAuditorium := validLiveSeatObservation(seatMapShowtime, "other-auditorium")
	if err := validateLiveSeatCapture(seatMapTask, mismatchedAuditorium); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("mismatched auditorium error = %v", err)
	}
	mismatchedAvailabilityAuditorium := validLiveSeatObservation(seatMapShowtime, seatMapAuditorium)
	mismatchedAvailabilityAuditorium.GetAvailability().SetAuditoriumId("other-auditorium")
	if err := validateLiveSeatCapture(seatMapTask, mismatchedAvailabilityAuditorium); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("mismatched availability auditorium error = %v", err)
	}
	mismatchedShowtime := validLiveSeatObservation("other-showtime", seatMapAuditorium)
	if err := validateLiveSeatCapture(seatMapTask, mismatchedShowtime); !errors.Is(err, errInvalidExecutionOutput) {
		t.Fatalf("mismatched showtime error = %v", err)
	}
	validAvailability := validLiveSeatObservation(availabilityShowtime, availabilityAuditorium)
	if err := validateLiveSeatCapture(availabilityTask, validAvailability); err != nil {
		t.Fatalf("valid availability capture rejected: %v", err)
	}
}

func TestAssignmentResultContractBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	runtime := newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	runtime.random = errorReader{}
	if _, err := runtime.assignmentResult(executionOutput{}, nil, now, now); err == nil {
		t.Fatalf("run ID failure = %v", err)
	}

	runtime = newRuntimeForTest(t, &fakeAPI{}, &fakeExecutor{})
	for _, test := range []struct {
		name string
		err  error
		want func(*observationpb.AssignmentResult) bool
	}{
		{name: "no bookable showtime", err: cgv.ErrNoBookableShowtime, want: func(result *observationpb.AssignmentResult) bool {
			return result.GetDeferred().GetReason().GetNoBookableShowtime() != nil
		}},
		{name: "target date", err: cgv.ErrTargetDateUnavailable, want: func(result *observationpb.AssignmentResult) bool {
			return result.GetDeferred().GetReason().GetTargetDateUnavailable() != nil
		}},
		{name: "browser start", err: cgv.ErrBrowserStartFailed, want: func(result *observationpb.AssignmentResult) bool {
			return result.GetFailed().GetReason().GetBrowserStartFailed() != nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := runtime.assignmentResult(executionOutput{}, test.err, now, now.Add(time.Second))
			if err != nil || !test.want(result) {
				t.Fatalf("assignmentResult() = %+v, %v", result, err)
			}
		})
	}
	if captureDeferredReason(errors.New("other")) != nil {
		t.Fatal("unclassified failure was deferred")
	}
	if captureFailureReason(cgv.ErrBrowserStartFailed).GetBrowserStartFailed() == nil {
		t.Fatal("browser startup failure was not classified")
	}
}

func TestResultOutcomeBoundaries(t *testing.T) {
	t.Parallel()
	deferred := &observationpb.AssignmentResult{}
	deferred.SetDeferred(&observationpb.Deferred{})
	failed := &observationpb.AssignmentResult{}
	failed.SetFailed(&observationpb.Failed{})
	completedWithoutPayload := &observationpb.AssignmentResult{}
	completedWithoutPayload.SetCompleted(&observationpb.Completed{})
	emptySchedule := &observationpb.ScheduleCaptures{}
	completedWithEmptySchedule := &observationpb.Completed{}
	completedWithEmptySchedule.SetSchedule(emptySchedule)
	emptyScheduleResult := &observationpb.AssignmentResult{}
	emptyScheduleResult.SetCompleted(completedWithEmptySchedule)
	if resultOutcome(nil) != "failed" || resultOutcome(deferred) != "deferred" ||
		resultOutcome(failed) != "failed" || resultOutcome(completedWithoutPayload) != "failed" ||
		resultOutcome(emptyScheduleResult) != "failed" {
		t.Fatal("assignment outcome boundary classification mismatch")
	}
}
