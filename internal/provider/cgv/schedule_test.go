package cgv

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseScheduleResponseUsesProviderIdentity(t *testing.T) {
	t.Parallel()
	rows, err := parseScheduleResponse([]byte(scheduleResponseFixture("20260812")))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.SiteNo != "0056" || row.MovieNo != "00001234" || row.AuditoriumNo != "0007" ||
		row.Date != "2026-08-12" || row.Sequence != "0003" || row.StartClock != "2530" || row.EndClock != "2832" {
		t.Fatalf("provider row = %+v", row)
	}
	entry, err := scheduleEntryFromProviderRow(row, testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Showtime.MovieSourceKey != "00001234" ||
		entry.Showtime.AuditoriumSourceKey != "0056/0007" ||
		entry.Showtime.SourceKey != "0056/2026-08-12/0007/0003" {
		t.Fatalf("source identities = %+v", entry.Showtime)
	}
	if entry.Showtime.ID != CatalogID(ProviderCGV, "showtime", entry.Showtime.SourceKey) ||
		entry.Showtime.MovieID != CatalogID(ProviderCGV, "movie", entry.Showtime.MovieSourceKey) ||
		entry.Showtime.AuditoriumID != CatalogID(ProviderCGV, "auditorium", entry.Showtime.AuditoriumSourceKey) {
		t.Fatalf("canonical identities = %+v", entry.Showtime)
	}
}

func TestProviderClockLabelMatchesCGVDisplay(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"0700":  "07:00",
		"07:00": "07:00",
		"2530":  "25:30",
	} {
		got, err := providerClockLabel(input)
		if err != nil || got != want {
			t.Fatalf("providerClockLabel(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := providerClockLabel("2460"); err == nil {
		t.Fatal("providerClockLabel accepted an invalid minute")
	}
}

func TestScheduleEntryRejectsTheaterRelationshipMismatch(t *testing.T) {
	t.Parallel()
	row := providerScheduleRow{
		SiteNo: "0001", MovieNo: "00001234", AuditoriumNo: "0007", Date: "2026-08-12",
		Sequence: "0003", AuditoriumName: "IMAX관", StartClock: "2530", EndClock: "2832",
	}
	if _, err := scheduleEntryFromProviderRow(row, testScheduleTheater()); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("theater relationship error = %v", err)
	}
}

func TestProviderIdentityIsStableWhenDisplayMetadataChanges(t *testing.T) {
	t.Parallel()
	rows, err := parseScheduleResponse([]byte(scheduleResponseFixture("2026-08-12")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := scheduleEntryFromProviderRow(rows[0], testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	rows[0].MovieTitle = "표시 제목 변경"
	rows[0].AuditoriumName = "다른 표시 이름"
	rows[0].ProductNo = "different-product"
	rows[0].MovieFileNo = "different-file"
	second, err := scheduleEntryFromProviderRow(rows[0], testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	if first.Showtime.ID != second.Showtime.ID || first.Showtime.MovieID != second.Showtime.MovieID ||
		first.Showtime.AuditoriumID != second.Showtime.AuditoriumID {
		t.Fatalf("display metadata changed identity: first=%+v second=%+v", first.Showtime, second.Showtime)
	}
}

func TestProviderIdentitySplitsOnSourceTuple(t *testing.T) {
	t.Parallel()
	base := providerScheduleRow{SiteNo: "0056", MovieNo: "00001234", AuditoriumNo: "0007", Date: "2026-08-12", Sequence: "0003", AuditoriumName: "IMAX관", StartClock: "2530", EndClock: "2832"}
	baseEntry, err := scheduleEntryFromProviderRow(base, testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*providerScheduleRow)
	}{
		{"theater", func(row *providerScheduleRow) { row.SiteNo = "0001" }},
		{"movie", func(row *providerScheduleRow) { row.MovieNo = "00009999" }},
		{"auditorium", func(row *providerScheduleRow) { row.AuditoriumNo = "0008" }},
		{"date", func(row *providerScheduleRow) { row.Date = "2026-08-13" }},
		{"sequence", func(row *providerScheduleRow) { row.Sequence = "0004" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row := base
			test.mutate(&row)
			theater := testScheduleTheater()
			if row.SiteNo != theater.SourceKey {
				theater.SourceKey = row.SiteNo
				theater.ID = CatalogID(ProviderCGV, "theater", row.SiteNo)
			}
			entry, err := scheduleEntryFromProviderRow(row, theater)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "movie" {
				if entry.Showtime.MovieID == baseEntry.Showtime.MovieID {
					t.Fatalf("%s tuple did not split movie identity: %q", test.name, entry.Showtime.MovieID)
				}
				return
			}
			if entry.Showtime.ID == baseEntry.Showtime.ID {
				t.Fatalf("%s tuple did not split showtime identity: %q", test.name, entry.Showtime.ID)
			}
		})
	}
}

func TestParseScheduleResponseFailsClosedForMissingOrInvalidProviderFields(t *testing.T) {
	t.Parallel()
	fieldValues := map[string]string{
		"siteNo": "0056", "movNo": "00001234", "scnsNo": "0007", "scnYmd": "20260812",
		"scnSseq": "0003", "scnsrtTm": "2530", "scnendTm": "2832",
	}
	for _, field := range []string{"siteNo", "movNo", "scnsNo", "scnYmd", "scnSseq", "scnsrtTm", "scnendTm"} {
		t.Run("missing_"+field, func(t *testing.T) {
			payload := scheduleResponseFixture("20260812")
			payload = strings.Replace(payload, `"`+field+`":"`+fieldValues[field]+`",`, "", 1)
			if _, err := parseScheduleResponse([]byte(payload)); err == nil {
				t.Fatalf("missing %s accepted", field)
			}
		})
	}
	for _, invalidDate := range []string{"20261340", "2026/08/12", ""} {
		t.Run("invalid_date_"+invalidDate, func(t *testing.T) {
			payload := strings.Replace(scheduleResponseFixture("20260812"), `"scnYmd":"20260812"`, `"scnYmd":"`+invalidDate+`"`, 1)
			if _, err := parseScheduleResponse([]byte(payload)); err == nil {
				t.Fatalf("invalid date %q accepted", invalidDate)
			}
		})
	}
	if _, err := parseScheduleResponse([]byte(`{"statusCode":1,"statusMessage":"failed","data":[]}`)); err == nil {
		t.Fatal("non-zero provider status accepted")
	}
	if _, err := parseScheduleResponse([]byte(`{"statusCode":0}`)); err == nil {
		t.Fatal("missing provider data accepted")
	}
}

func TestScheduleDateMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{
		providerResponses: []capturedProviderResponse{{
			path:   scheduleResponsePath,
			status: 200,
			body:   []byte(scheduleResponseFixture("20260812")),
		}},
	}
	if _, err := adapter.extractSchedules("2026-08-13", testScheduleTheater()); err == nil {
		t.Fatal("schedule row from another date was silently dropped")
	}
}

func TestScheduleCaptureFiltersRowsFromAnotherDate(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(
		scheduleResponseFixture("20260812"),
		`]}`,
		`,{"siteNo":"0056","siteNm":"용산아이파크몰","movNo":"00005678","scnsNo":"0008","scnsNm":"4관","scnYmd":"20260813","scnSseq":"0004","scnsrtTm":"1100","scnendTm":"1300","frSeatCnt":"8","stcnt":"40"}]}`,
		1,
	)
	adapter := &Adapter{providerResponses: []capturedProviderResponse{{
		path: scheduleResponsePath, status: 200, body: []byte(payload),
	}}}
	entries, err := adapter.extractSchedules("2026-08-12", testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Showtime.Date != "2026-08-12" {
		t.Fatalf("target-date entries = %+v", entries)
	}
}

func TestScheduleCaptureFiltersLinkedVenueRows(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(
		scheduleResponseFixture("20260812"),
		`]}`,
		`,{"siteNo":"P013","siteNm":"씨네드쉐프 용산","movNo":"00005678","scnsNo":"0002","scnsNm":"템퍼 시네마","scnYmd":"20260812","scnSseq":"0004","scnsrtTm":"1100","scnendTm":"1300","frSeatCnt":"8","stcnt":"40"}]}`,
		1,
	)
	adapter := &Adapter{providerResponses: []capturedProviderResponse{{
		path: scheduleResponsePath, status: 200, body: []byte(payload),
	}}}
	entries, err := adapter.extractSchedules("2026-08-12", testScheduleTheater())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Showtime.ProviderID != ProviderCGV ||
		entries[0].Showtime.TheaterID != testScheduleTheater().ID {
		t.Fatalf("target theater entries = %+v", entries)
	}
}

func TestScheduleCaptureRejectsOnlyLinkedVenueRows(t *testing.T) {
	t.Parallel()
	payload := strings.Replace(scheduleResponseFixture("20260812"), `"siteNo":"0056"`, `"siteNo":"P013"`, 1)
	adapter := &Adapter{providerResponses: []capturedProviderResponse{{
		path: scheduleResponsePath, status: 200, body: []byte(payload),
	}}}
	if _, err := adapter.extractSchedules("2026-08-12", testScheduleTheater()); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("linked-only response error = %v", err)
	}
}

func TestScheduleDateNormalizesBeforeCapture(t *testing.T) {
	for _, input := range []string{"20260812", "2026-08-12"} {
		canonical, err := canonicalProviderDate(input)
		if err != nil || canonical != "2026-08-12" {
			t.Fatalf("canonicalProviderDate(%q) = %q, %v", input, canonical, err)
		}
	}
}

func TestParseProviderClockRange(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("KST", 9*60*60)
	startsAt, endsAt, err := ParseShowtimeRange("2026-08-20", "2530", "2832", location)
	if err != nil {
		t.Fatal(err)
	}
	if startsAt.Format(time.DateTime) != "2026-08-21 01:30:00" || endsAt.Format(time.DateTime) != "2026-08-21 04:32:00" {
		t.Fatalf("extended-hour range = %s - %s", startsAt, endsAt)
	}
	startsAt, endsAt, err = ParseShowtimeRange("2026-08-20", "23:50", "00:20", location)
	if err != nil || endsAt.Sub(startsAt) != 30*time.Minute || endsAt.Day() != 21 {
		t.Fatalf("ordinary overnight range = %v - %v, %v", startsAt, endsAt, err)
	}
	for _, clock := range []string{"bad", "2860", "4800"} {
		if _, _, err := ParseShowtimeRange("2026-08-20", clock, "11:00", location); err == nil {
			t.Fatalf("invalid clock %q accepted", clock)
		}
	}
}

func TestScheduleResponseURLIsExact(t *testing.T) {
	t.Parallel()
	if !scheduleResponseURL("https://cgv.co.kr/api/v1/booking/searchMovScnInfo?siteNo=0056") {
		t.Fatal("current schedule response URL rejected")
	}
	for _, rawURL := range []string{
		"https://cgv.co.kr/cnm/atkt/searchMovScnInfo?siteNo=0056",
	} {
		if scheduleResponseURL(rawURL) {
			t.Fatalf("legacy response URL accepted: %q", rawURL)
		}
	}
	for _, rawURL := range []string{
		"https://cgv.co.kr/api/v1/booking/searchMovScnInfoFake",
		"https://example.invalid/api/v1/booking/searchMovScnInfo",
		"://invalid",
	} {
		if scheduleResponseURL(rawURL) {
			t.Fatalf("untrusted response URL accepted: %q", rawURL)
		}
	}
}

func TestDateSelectionLabelsPreserveTodayAndWeekdayVariants(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("KST", 9*60*60)
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, location)
	today := date.Add(8 * time.Hour)
	labels := dateSelectionLabels(date, today)
	for _, want := range []string{"오늘 12", "오늘12", "12 오늘", "12오늘", "수 12", "수12", "12 수", "12수"} {
		if !containsLabel(labels, want) {
			t.Fatalf("today label %q missing from %v", want, labels)
		}
	}
	other := dateSelectionLabels(date, date.Add(24*time.Hour))
	for _, forbidden := range []string{"오늘 12", "오늘12", "12 오늘", "12오늘"} {
		if containsLabel(other, forbidden) {
			t.Fatalf("today-only label %q used for another date", forbidden)
		}
	}
}

func TestAvailableScheduleDatesUsesEveryVisibleProviderDate(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("KST", 9*60*60)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, location)
	dates, err := availableScheduleDatesFromLabels([]string{
		"오늘 24", "화 25", "수 26", "로그인", "극장선택",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-08-24", "2026-08-25", "2026-08-26"}
	if len(dates) != len(want) {
		t.Fatalf("available dates = %v, want %v", dates, want)
	}
	for index := range want {
		if dates[index] != want[index] {
			t.Fatalf("available dates = %v, want %v", dates, want)
		}
	}
	if _, err := availableScheduleDatesFromLabels([]string{"로그인"}, now); !errors.Is(err, ErrUIContractChanged) {
		t.Fatalf("missing dates error = %v", err)
	}
}

func TestFilterScheduleDatesByWeekdaysKeepsOnlyMonitorProviderDays(t *testing.T) {
	t.Parallel()
	dates := []string{"2026-08-24", "2026-08-25", "2026-08-26", "2026-08-27"}
	filtered, err := filterScheduleDatesByWeekdays(dates, []time.Weekday{time.Monday, time.Wednesday})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-08-24", "2026-08-26"}
	if len(filtered) != len(want) || filtered[0] != want[0] || filtered[1] != want[1] {
		t.Fatalf("filtered dates = %v, want %v", filtered, want)
	}
	if _, err := filterScheduleDatesByWeekdays(dates, []time.Weekday{time.Weekday(7)}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("invalid weekday error = %v", err)
	}
}

func TestSelectedDateMarkerMatchesCurrentCGVContract(t *testing.T) {
	t.Parallel()
	if !selectedButtonTitle("선택됨") {
		t.Fatal("current selected button title was rejected")
	}
	for _, title := range []string{"", "선택", "selected"} {
		if selectedButtonTitle(title) {
			t.Fatalf("non-selected button title %q was accepted", title)
		}
	}
}

func TestScheduleResponseReadyWaitUsesCapturedResponse(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{
		ctx: context.Background(),
		providerResponses: []capturedProviderResponse{{
			path: scheduleResponsePath, status: 200, body: []byte(scheduleResponseFixture("20260812")),
		}},
	}
	startedAt := time.Now()
	if err := adapter.waitForScheduleResponseReady("2026-08-12", time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("already captured response waited %s", elapsed)
	}
	if len(adapter.providerResponses) != 1 {
		t.Fatal("readiness wait consumed the provider response")
	}
}

func TestScheduleResponseReadyWaitStopsAtTimeoutOrBrowserCancellation(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{ctx: context.Background()}
	if err := adapter.waitForScheduleResponseReady("2026-08-12", 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	adapter.ctx = cancelled
	if err := adapter.waitForScheduleResponseReady("2026-08-12", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestScheduleResponseReadyWaitIgnoresLatePreviousDate(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{
		ctx: context.Background(),
		providerResponses: []capturedProviderResponse{{
			path: scheduleResponsePath, status: 200, body: []byte(scheduleResponseFixture("20260812")),
		}},
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		adapter.scheduleResponseMu.Lock()
		adapter.providerResponses = append(adapter.providerResponses, capturedProviderResponse{
			path: scheduleResponsePath, status: 200, body: []byte(scheduleResponseFixture("20260813")),
		})
		adapter.scheduleResponseMu.Unlock()
	}()
	startedAt := time.Now()
	if err := adapter.waitForScheduleResponseReady("2026-08-13", time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed < 20*time.Millisecond {
		t.Fatalf("late response from the previous date ended the wait after %s", elapsed)
	}
}

func TestScheduleResponseReadyAcceptsEmptyResponseForTargetURL(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{
		ctx: context.Background(),
		providerResponses: []capturedProviderResponse{{
			path:       scheduleResponsePath,
			requestURL: "https://cgv.co.kr/api/v1/booking/searchMovScnInfo?siteNo=0013&scnYmd=20260813",
			status:     200,
			body:       []byte(`{"statusCode":0,"statusMessage":"success","data":[]}`),
		}},
	}
	if err := adapter.waitForScheduleResponseReady("2026-08-13", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleTheaterSelectionReusesCurrentPage(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{ctx: context.Background(), selectedRegion: "서울", selectedTheater: "용산아이파크몰"}
	if err := adapter.selectCinemaTheater("서울", "용산아이파크몰"); err != nil {
		t.Fatalf("current theater selection was not reused: %v", err)
	}
}

func testScheduleTheater() ScheduleTheater {
	return ScheduleTheater{
		ID:         CatalogID(ProviderCGV, "theater", "0056"),
		ProviderID: ProviderCGV, SourceKey: "0056", Region: "서울", Name: "용산아이파크몰",
	}
}

func scheduleResponseFixture(date string) string {
	return `{"statusCode":0,"statusMessage":"success","data":[{"siteNo":"0056","siteNm":"용산아이파크몰","movNo":"00001234","movfNo":"file-01","movNm":"표시 영화","scnsNo":"0007","scnsNm":"IMAX관","scnYmd":"` + date + `","scnSseq":"0003","prodNo":"product-01","scnsrtTm":"2530","scnendTm":"2832","frSeatCnt":"2","stcnt":"624"}]}`
}

func containsLabel(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
