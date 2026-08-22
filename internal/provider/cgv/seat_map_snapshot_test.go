package cgv

import (
	"reflect"
	"strings"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSeatMapTaskRequiresCanonicalExactShowtime(t *testing.T) {
	t.Parallel()
	theaterSource := "0056"
	theaterID := CatalogID(ProviderCGV, "theater", theaterSource)
	auditoriumSource := theaterSource + "/0007"
	auditoriumID := CatalogID(ProviderCGV, "auditorium", auditoriumSource)
	showtimeSource := theaterSource + "/2026-08-21/0007/0003"
	showtimeID := CatalogID(ProviderCGV, "showtime", showtimeSource)
	startsAt := time.Date(2026, 8, 21, 20, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	theater := &catalogpb.Theater{}
	theater.SetId(theaterID)
	theater.SetProviderId(ProviderCGV)
	theater.SetSourceKey(theaterSource)
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(auditoriumID)
	auditorium.SetTheaterId(theaterID)
	auditorium.SetSourceKey(auditoriumSource)
	auditorium.SetName("IMAX관")
	movie := &catalogpb.Movie{}
	movie.SetId(CatalogID(ProviderCGV, "movie", "00001234"))
	movie.SetProviderId(ProviderCGV)
	movie.SetSourceKey("00001234")
	movie.SetTitle("Example Movie")
	showtime := &catalogpb.Showtime{}
	showtime.SetId(showtimeID)
	showtime.SetProviderId(ProviderCGV)
	showtime.SetSourceKey(showtimeSource)
	showtime.SetTheaterId(theaterID)
	showtime.SetAuditorium(auditorium)
	showtime.SetMovie(movie)
	scheduleDate := &commonpb.LocalDate{}
	scheduleDate.SetYear(2026)
	scheduleDate.SetMonth(8)
	scheduleDate.SetDay(21)
	showtime.SetScheduleDate(scheduleDate)
	showtime.SetStartsAt(timestamppb.New(startsAt))
	showtime.SetEndsAt(timestamppb.New(startsAt.Add(2 * time.Hour)))
	seatTask := &observationpb.SeatMapTask{}
	seatTask.SetTheater(theater)
	seatTask.SetAuditorium(auditorium)
	seatTask.SetShowtime(showtime)
	seatTask.SetTimeZone("Asia/Seoul")
	task := &observationpb.AssignmentTask{}
	task.SetSeatMap(seatTask)
	egress := &commonpb.EgressPolicy{}
	egress.SetManagedScan(&commonpb.ManagedScanEgress{})
	task.SetEgress(egress)
	if err := validateSeatMapTask(task); err != nil {
		t.Fatalf("canonical task rejected: %v", err)
	}
	entry := scheduleEntry{Showtime: ScheduleShowtime{
		ID: showtimeID, ProviderID: ProviderCGV, SourceKey: showtimeSource, TheaterID: theaterID,
		MovieID: task.GetSeatMap().GetShowtime().GetMovie().GetId(), AuditoriumID: auditoriumID,
	}}
	if _, err := exactSeatMapShowtime([]scheduleEntry{entry}, task.GetSeatMap().GetShowtime()); err != nil {
		t.Fatalf("exact provider showtime rejected: %v", err)
	}
	task.GetSeatMap().GetShowtime().GetScheduleDate().SetYear(0)
	if err := validateSeatMapTask(task); err == nil {
		t.Fatal("year-zero exact showtime accepted")
	}
	task.GetSeatMap().GetShowtime().GetScheduleDate().SetYear(2026)
	task.GetSeatMap().GetShowtime().SetSourceKey("")
	if err := validateSeatMapTask(task); err == nil {
		t.Fatal("noncanonical showtime accepted")
	}
}

func TestSeatMapTaskAcceptsExploratoryDateWindow(t *testing.T) {
	t.Parallel()
	theaterSource := "0056"
	theaterID := CatalogID(ProviderCGV, "theater", theaterSource)
	auditoriumSource := theaterSource + "/0007"
	auditoriumID := CatalogID(ProviderCGV, "auditorium", auditoriumSource)
	theater := &catalogpb.Theater{}
	theater.SetId(theaterID)
	theater.SetProviderId(ProviderCGV)
	theater.SetSourceKey(theaterSource)
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(auditoriumID)
	auditorium.SetTheaterId(theaterID)
	auditorium.SetSourceKey(auditoriumSource)
	auditorium.SetName("IMAX관")
	targetDate := &commonpb.LocalDate{}
	targetDate.SetYear(2026)
	targetDate.SetMonth(8)
	targetDate.SetDay(21)
	seatTask := &observationpb.SeatMapTask{}
	seatTask.SetTheater(theater)
	seatTask.SetAuditorium(auditorium)
	seatTask.SetTargetDates([]*commonpb.LocalDate{targetDate})
	seatTask.SetTimeZone("Asia/Seoul")
	task := &observationpb.AssignmentTask{}
	task.SetSeatMap(seatTask)
	if err := validateSeatMapTask(task); err != nil {
		t.Fatalf("exploratory task rejected: %v", err)
	}
	seatTask.SetTargetDates(nil)
	if err := validateSeatMapTask(task); err == nil {
		t.Fatal("seat-map task without showtime or dates accepted")
	}
}

func TestFirstBookableSeatMapShowtimeMatchesAuditorium(t *testing.T) {
	t.Parallel()
	entries := []scheduleEntry{
		{Showtime: ScheduleShowtime{ID: "wrong-auditorium", AuditoriumID: "auditorium-2", AvailableSeats: 20}},
		{Showtime: ScheduleShowtime{ID: "sold-out", AuditoriumID: "auditorium-1", SoldOut: true}},
		{Showtime: ScheduleShowtime{ID: "bookable", AuditoriumID: "auditorium-1", AvailableSeats: 1}},
	}
	showtime, found := firstBookableSeatMapShowtime(entries, "auditorium-1")
	if !found || showtime.ID != "bookable" {
		t.Fatalf("bookable showtime = %+v, %v", showtime, found)
	}
}

func TestParseSeatMapLayoutPreservesStaticSemantics(t *testing.T) {
	t.Parallel()
	layout, err := parseSeatMapLayout([]byte(seatMapFixture), "auditorium-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.GetSeats()) != 2 || len(layout.GetZones()) != 1 || len(layout.GetBlocks()) != 1 {
		t.Fatalf("layout counts = seats:%d zones:%d blocks:%d", len(layout.GetSeats()), len(layout.GetZones()), len(layout.GetBlocks()))
	}
	seat := layout.GetSeats()[0]
	if seat.GetLabel() != "A19" || seat.GetId() != SeatID("auditorium-1", "A19") ||
		seat.GetType() != "wheelchair" || seat.GetSaleFormName() != "이동식" || !seat.GetRightAisle() {
		t.Fatalf("first seat = %+v", seat)
	}
	if seat.GetX() <= 0.40 || seat.GetX() >= 0.45 || seat.GetY() <= 0 || seat.GetY() >= 0.1 {
		t.Fatalf("normalized position = %.4f,%.4f", seat.GetX(), seat.GetY())
	}
	if !reflect.DeepEqual(seat.GetFeatures(), []string{
		"removable", "right-aisle", "sale-form:이동식", "wheelchair-area", "zone:Light존",
	}) {
		t.Fatalf("canonical features = %v", seat.GetFeatures())
	}
	hash, err := layoutHash(layout)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "c64db650f1c11a6de3988acbdd5a92b1cbe01115835073c9c0bc080c2b6734f8" {
		t.Fatalf("canonical fixture layout hash = %s", hash)
	}
}

func TestParseSeatMapLayoutRejectsIncompleteProviderData(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"invalid JSON":   `{`,
		"provider error": `{"statusCode":1,"resultMsg":"blocked"}`,
		"no items":       `{"statusCode":0,"data":{"items":[]}}`,
		"no seats":       `{"statusCode":0,"data":{"items":[{"sbord":{"xcoordStartVal":"0","ycoordStartVal":"0","xcoordEndVal":"1","ycoordEndVal":"1"}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSeatMapLayout([]byte(body), "auditorium-1"); err == nil {
				t.Fatal("invalid provider response accepted")
			}
		})
	}
}

func TestParseSeatMapLayoutRejectsCorruptCoordinatesAndCapacity(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"non-finite board":        strings.Replace(seatMapFixture, `"xcoordStartVal":"0000"`, `"xcoordStartVal":"NaN"`, 1),
		"invalid seat coordinate": strings.Replace(seatMapFixture, `"xcoordStartVal":"0041"`, `"xcoordStartVal":"broken"`, 1),
		"seat outside board":      strings.Replace(seatMapFixture, `"xcoordEndVal":"0043"`, `"xcoordEndVal":"9999"`, 1),
		"invalid zone capacity":   strings.Replace(seatMapFixture, `"maxNopsn":"2"`, `"maxNopsn":"many"`, 1),
		"empty seat row":          strings.Replace(seatMapFixture, `"seatRowNm":"A"`, `"seatRowNm":""`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSeatMapLayout([]byte(body), "auditorium-1"); err == nil {
				t.Fatal("corrupt provider seat map was accepted")
			}
		})
	}
}

const seatMapFixture = `{
  "statusCode": 0,
  "resultMsg": "Success",
  "data": {"items": [{
    "sbord": {"xcoordStartVal":"0000","ycoordStartVal":"0000","xcoordEndVal":"0101","ycoordEndVal":"0035","stcnt":2},
    "salfrms": [
      {"seatSalfrmCd":"01","seatSalfrmNm":"일반"},
      {"seatSalfrmCd":"04","seatSalfrmNm":"이동식"}
    ],
    "szones": [
      {"szoneNo":"02001","szoneNm":"Light존","szoneKindCd":"02","szoneKindNm":"Light존","xcoordStartVal":"0007","ycoordStartVal":"0001","xcoordEndVal":"0093","ycoordEndVal":"0005","maxNopsn":"2"}
    ],
    "sblcks": [
      {"sblckNo":"01001","sblckNm":"입구","sblckKindCd":"01","sblckKindNm":"입구","xcoordStartVal":"0067","ycoordStartVal":"0000","xcoordEndVal":"0068","ycoordEndVal":"0001"}
    ],
    "seats": [
      {"seatRowNm":"A","seatNo":"19","stkndCd":"01","stkndNm":"일반석","szoneNm":"Light존","szoneKindNm":"Light존","seatSalfrmCd":"04","xcoordStartVal":"0041","ycoordStartVal":"0001","xcoordEndVal":"0043","ycoordEndVal":"0003","leftPwayYn":"N","rghtPwayYn":"Y"},
      {"seatRowNm":"A","seatNo":"22","stkndCd":"01","stkndNm":"일반석","szoneNm":"Light존","szoneKindNm":"Light존","seatSalfrmCd":"04","xcoordStartVal":"0047","ycoordStartVal":"0001","xcoordEndVal":"0049","ycoordEndVal":"0003","leftPwayYn":"Y","rghtPwayYn":"N"}
    ]
  }]}
}`
