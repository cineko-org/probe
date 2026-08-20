package cgv

import (
	"bytes"
	"testing"

	contracts "github.com/cineko-org/contracts/v3"
)

func TestProviderResponseClearPreventsStaleReuse(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{providerResponses: map[string][]byte{"schedules": []byte("old")}}
	if got := adapter.providerResponse("schedules"); !bytes.Equal(got, []byte("old")) {
		t.Fatalf("initial provider response = %q", got)
	}
	adapter.clearProviderResponse("schedules")
	if got := adapter.providerResponse("schedules"); len(got) != 0 {
		t.Fatalf("stale provider response survived clear: %q", got)
	}
}

const syntheticSchedulePrefix = `{"data":[`

func syntheticScheduleJSON(site, screen, seq, movie, product, format, title, start string) []byte {
	return []byte(syntheticSchedulePrefix +
		`{"siteNo":"` + site + `","scnsNo":"` + screen + `","scnYmd":"2026-08-20","scnSseq":"` + seq +
		`","movNo":"` + movie + `","prodNo":"` + product + `","movfNo":"` + format +
		`","movNm":"` + title + `","scnsNm":"IMAX관","scnsrtTm":"` + start +
		`","scnendTm":"16:00","frSeatCnt":"12","stcnt":"624"}]}`)
}

func syntheticTheater() ScheduleTheater {
	return ScheduleTheater{
		ID: contracts.CatalogID(contracts.ProviderCGV, "theater", "0013"), ProviderID: contracts.ProviderCGV,
		SourceKey: "0013", Region: "서울", Name: "용산아이파크몰",
	}
}

func TestCGVProviderIdentityIgnoresDisplayChanges(t *testing.T) {
	t.Parallel()
	theater := syntheticTheater()
	first, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "3", "30001323", "10", "20", "원래 제목", "14:00"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "3", "30001323", "99", "77", "제목 (무대인사)", "14:05"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Showtime.ID != second[0].Showtime.ID ||
		first[0].Showtime.MovieID != second[0].Showtime.MovieID ||
		first[0].Showtime.AuditoriumID != second[0].Showtime.AuditoriumID {
		t.Fatalf("provider identity changed with labels: first=%+v second=%+v", first[0].Showtime, second[0].Showtime)
	}
}

func TestCGVProviderIdentitySeparatesMovieAuditoriumAndShowtime(t *testing.T) {
	t.Parallel()
	theater := syntheticTheater()
	base, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "3", "30001323", "10", "20", "같은 표시", "14:00"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	otherMovie, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "3", "30009999", "10", "20", "같은 표시", "14:00"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	otherScreen, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "019", "3", "30001323", "10", "20", "같은 표시", "14:00"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	otherShowtime, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "4", "30001323", "10", "20", "같은 표시", "14:00"), "2026-08-20", theater)
	if err != nil {
		t.Fatal(err)
	}
	if base[0].Showtime.MovieID == otherMovie[0].Showtime.MovieID {
		t.Fatal("different movNo values were merged")
	}
	if base[0].Showtime.AuditoriumID == otherScreen[0].Showtime.AuditoriumID {
		t.Fatal("different scnsNo values were merged")
	}
	if base[0].Showtime.ID == otherShowtime[0].Showtime.ID {
		t.Fatal("different scnSseq values were merged")
	}
}

func TestCGVProviderIdentityRejectsIncompleteCandidate(t *testing.T) {
	t.Parallel()
	theater := syntheticTheater()
	if _, err := parseCGVScheduleResponse(syntheticScheduleJSON("0013", "018", "", "30001323", "10", "20", "제목", "14:00"), "2026-08-20", theater); err == nil {
		t.Fatal("incomplete provider tuple was accepted")
	}
}

func TestCGVProviderAvailabilityMarksZeroRemainingSeatsSoldOut(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"data":[{"siteNo":"0013","scnsNo":"018","scnYmd":"2026-08-20","scnSseq":"3","movNo":"30001323","movNm":"영화","scnsNm":"IMAX관","scnsrtTm":"1400","scnendTm":"1600","frSeatCnt":"0","stcnt":"624"}]}`)
	entries, err := parseCGVScheduleResponse(payload, "2026-08-20", syntheticTheater())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Showtime.SoldOut {
		t.Fatalf("zero-remaining showtime = %+v", entries)
	}
}

func TestCGVProviderNormalizesCompactScheduleDateForIdentity(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"data":[{"siteNo":"0013","scnsNo":"018","scnYmd":"20260821","scnSseq":"3","movNo":"30001323","movNm":"심야 영화","scnsNm":"IMAX관","scnsrtTm":"2500","scnendTm":"2709","frSeatCnt":"12","stcnt":"624"}]}`)
	entries, err := parseCGVScheduleResponse(payload, "2026-08-21", syntheticTheater())
	if err != nil {
		t.Fatal(err)
	}
	showtime := entries[0].Showtime
	if showtime.Date != "2026-08-21" || showtime.StartsAt != "25:00" || showtime.EndsAt != "27:09" ||
		showtime.SourceKey != "0013/2026-08-21/018/3" ||
		showtime.ID != contracts.CatalogID(contracts.ProviderCGV, "showtime", "0013/2026-08-21/018/3") {
		t.Fatalf("compact schedule identity = %+v", showtime)
	}
}

func TestCGVProviderRejectsInvalidScheduleDate(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"data":[{"siteNo":"0013","scnsNo":"018","scnYmd":"20261340","scnSseq":"3","movNo":"30001323","movNm":"영화","scnsNm":"IMAX관","scnsrtTm":"2500","scnendTm":"2709","frSeatCnt":"12","stcnt":"624"}]}`)
	if _, err := parseCGVScheduleResponse(payload, "2026-08-21", syntheticTheater()); err == nil {
		t.Fatal("invalid compact schedule date was accepted")
	}
}

func TestCGVSiteIdentityUsesSiteNo(t *testing.T) {
	t.Parallel()
	first, err := parseCGVSiteResponse([]byte(`{"data":{"siteInfo":[{"regnGrpCd":"01","siteNo":"0013","siteNm":"용산아이파크몰"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseCGVSiteResponse([]byte(`{"data":{"siteInfo":[{"regnGrpCd":"01","siteNo":"0013","siteNm":"용산 아이파크몰 (리뉴얼)"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if first[0].SourceKey != second[0].SourceKey || first[0].SourceKey != "0013" || first[0].Region != "서울" {
		t.Fatalf("site identity = %q, %q", first[0].SourceKey, second[0].SourceKey)
	}
}
