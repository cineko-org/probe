package cgv

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestParseSeatAvailabilityNormalizesLiveSeatIDs(t *testing.T) {
	t.Parallel()
	body := strings.Replace(seatAvailabilityFixture, `"seatNo":"19"`, `"seatNo":"019"`, 1)
	layout, err := parseSeatMapLayout([]byte(body), "auditorium-1")
	if err != nil {
		t.Fatal(err)
	}
	available, err := parseSeatAvailability([]byte(body), "auditorium-1", layout)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{SeatID("auditorium-1", "A19")}
	if !slices.Equal(available, want) {
		t.Fatalf("available seats = %v, want %v", available, want)
	}
}

func TestParseSeatAvailabilityFailsClosedForIncompleteStatus(t *testing.T) {
	t.Parallel()
	layout, err := parseSeatMapLayout([]byte(seatAvailabilityFixture), "auditorium-1")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing sale flag": strings.Replace(seatAvailabilityFixture, `"seatSaleYn":"N"`, `"seatSaleYn":""`, 1),
		"missing status":    strings.Replace(seatAvailabilityFixture, `"seatStusCd":"04"`, `"seatStusCd":""`, 1),
		"unknown status":    strings.Replace(seatAvailabilityFixture, `"seatStusCd":"04"`, `"seatStusCd":"99"`, 1),
		"partial response":  strings.Replace(seatAvailabilityFixture, `"seatRowNm":"A","seatNo":"22"`, `"seatRowNm":"B","seatNo":"22"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseSeatAvailability([]byte(body), "auditorium-1", layout)
			if !errors.Is(err, ErrSeatAvailabilityIncomplete) {
				t.Fatalf("parseSeatAvailability() error = %v", err)
			}
		})
	}
}

func TestSeatAvailabilityTaskRequiresOneExactShowtime(t *testing.T) {
	t.Parallel()
	theaterID := CatalogID(ProviderCGV, "theater", "0056")
	auditoriumID := CatalogID(ProviderCGV, "auditorium", "0056/0007")
	theater := &catalogpb.Theater{}
	theater.SetId(theaterID)
	theater.SetProviderId(ProviderCGV)
	theater.SetIdentity(NewTheaterIdentity("0056"))
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	auditorium := &catalogpb.Auditorium{}
	auditorium.SetId(auditoriumID)
	auditorium.SetTheaterId(theaterID)
	auditorium.SetIdentity(NewAuditoriumIdentity("0056", "0007"))
	auditorium.SetName("IMAX관")
	movie := &catalogpb.Movie{}
	movie.SetId(CatalogID(ProviderCGV, "movie", "00001234"))
	movie.SetProviderId(ProviderCGV)
	movie.SetIdentity(NewMovieIdentity("00001234"))
	movie.SetTitle("Example Movie")
	showtime := &catalogpb.Showtime{}
	showtime.SetId(CatalogID(ProviderCGV, "showtime", "0056/2026-08-21/0007/0003"))
	showtime.SetProviderId(ProviderCGV)
	showtime.SetTheaterId(theaterID)
	showtime.SetMovie(movie)
	showtime.SetAuditorium(auditorium)
	showtimeIdentity, err := NewShowtimeIdentity("0056", "2026-08-21", "0007", "0003")
	if err != nil {
		t.Fatal(err)
	}
	showtime.SetIdentity(showtimeIdentity)
	startsAt := time.Date(2026, 8, 21, 20, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	showtime.SetStartsAt(timestamppb.New(startsAt))
	showtime.SetEndsAt(timestamppb.New(startsAt.Add(2 * time.Hour)))
	task := &observationpb.SeatAvailabilityTask{}
	task.SetTheater(theater)
	task.SetAuditorium(auditorium)
	task.SetShowtime(showtime)
	task.SetLocale("ko-KR")
	task.SetTimeZone("Asia/Seoul")
	assignment := &observationpb.AssignmentTask{}
	assignment.SetSeatAvailability(task)
	egress := &commonpb.EgressPolicy{}
	egress.SetManagedScan(&commonpb.ManagedScanEgress{})
	assignment.SetEgress(egress)
	if err := validateSeatAvailabilityTask(assignment); err != nil {
		t.Fatalf("exact showtime task rejected: %v", err)
	}
	assignment.GetSeatAvailability().ClearShowtime()
	if err := validateSeatAvailabilityTask(assignment); err == nil {
		t.Fatal("seat-availability task without showtime accepted")
	}
}

const seatAvailabilityFixture = `{
  "statusCode": 0,
  "resultMsg": "Success",
  "data": {"items": [{
    "sbord": {"xcoordStartVal":"0000","ycoordStartVal":"0000","xcoordEndVal":"0101","ycoordEndVal":"0035","stcnt":2},
    "salfrms": [{"seatSalfrmCd":"01","seatSalfrmNm":"일반"}],
    "seats": [
      {"seatLocNo":"1","seatRowNm":"A","seatNo":"19","stkndCd":"01","stkndNm":"일반석","seatSalfrmCd":"01","seatStusCd":"00","seatStusNm":"미정","seatSaleYn":"Y","xcoordStartVal":"0041","ycoordStartVal":"0001","xcoordEndVal":"0043","ycoordEndVal":"0003"},
      {"seatLocNo":"2","seatRowNm":"A","seatNo":"22","stkndCd":"01","stkndNm":"일반석","seatSalfrmCd":"01","seatStusCd":"04","seatStusNm":"진행","seatSaleYn":"N","xcoordStartVal":"0047","ycoordStartVal":"0001","xcoordEndVal":"0049","ycoordEndVal":"0003"}
    ]
  }]}
}`
