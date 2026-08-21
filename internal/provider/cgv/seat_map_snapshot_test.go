package cgv

import (
	"strings"
	"testing"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

func TestSeatMapTaskRequiresCanonicalExactShowtime(t *testing.T) {
	t.Parallel()
	theaterSource := "0056"
	theaterID := contracts.CatalogID(contracts.ProviderCGV, "theater", theaterSource)
	auditoriumSource := theaterSource + "/0007"
	auditoriumID := contracts.CatalogID(contracts.ProviderCGV, "auditorium", auditoriumSource)
	showtimeSource := theaterSource + "/2026-08-21/0007/0003"
	showtimeID := contracts.CatalogID(contracts.ProviderCGV, "showtime", showtimeSource)
	startsAt := time.Date(2026, 8, 21, 20, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	task := contracts.AssignmentTask{
		Kind: contracts.CapabilityCGVSeatMapCapture,
		Theater: contracts.Theater{
			ID: theaterID, ProviderID: contracts.ProviderCGV, SourceKey: theaterSource,
			Region: "서울", Name: "용산아이파크몰",
		},
		Auditorium: &contracts.Auditorium{
			ID: auditoriumID, TheaterID: theaterID, SourceKey: auditoriumSource, Name: "IMAX관",
		},
		Showtime: &contracts.Showtime{
			ID: showtimeID, ProviderID: contracts.ProviderCGV, SourceKey: showtimeSource,
			Auditorium: contracts.Auditorium{ID: auditoriumID},
			Movie:      contracts.Movie{ID: contracts.CatalogID(contracts.ProviderCGV, "movie", "00001234")},
			StartsAt:   startsAt, EndsAt: startsAt.Add(2 * time.Hour),
		},
		TimeZone: "Asia/Seoul",
	}
	if err := validateSeatMapTask(task); err != nil {
		t.Fatalf("canonical task rejected: %v", err)
	}
	entry := scheduleEntry{Showtime: ScheduleShowtime{
		SourceKey: showtimeSource, MovieID: task.Showtime.Movie.ID, AuditoriumID: auditoriumID,
	}}
	if _, err := exactSeatMapShowtime([]scheduleEntry{entry}, *task.Showtime); err != nil {
		t.Fatalf("exact provider showtime rejected: %v", err)
	}
	task.Showtime.SourceKey = ""
	if err := validateSeatMapTask(task); err == nil {
		t.Fatal("noncanonical showtime accepted")
	}
}

func TestParseSeatMapLayoutPreservesStaticSemantics(t *testing.T) {
	t.Parallel()
	layout, err := parseSeatMapLayout([]byte(seatMapFixture), "auditorium-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Seats) != 2 || len(layout.Zones) != 1 || len(layout.Blocks) != 1 {
		t.Fatalf("layout counts = seats:%d zones:%d blocks:%d", len(layout.Seats), len(layout.Zones), len(layout.Blocks))
	}
	seat := layout.Seats[0]
	if seat.Label != "A19" || seat.ID != contracts.SeatID("auditorium-1", "A19") ||
		seat.Type != "wheelchair" || seat.SaleFormName != "이동식" || !seat.RightAisle {
		t.Fatalf("first seat = %+v", seat)
	}
	if seat.X <= 0.40 || seat.X >= 0.45 || seat.Y <= 0 || seat.Y >= 0.1 {
		t.Fatalf("normalized position = %.4f,%.4f", seat.X, seat.Y)
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
