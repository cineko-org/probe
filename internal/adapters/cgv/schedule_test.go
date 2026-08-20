package cgv

import (
	"testing"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

func TestParseScheduleCanonicalStoredCGVIdentity(t *testing.T) {
	t.Parallel()
	entry, ok := parseSchedule(rawSchedule{
		Label:      "19:45 - 21:44 매진 조조 성우 무대인사 컬처데이",
		Movie:      "명탐정 코난-하이웨이의 타천사",
		Group:      "2D",
		Auditorium: "6관 (Laser)",
		Disabled:   true,
		SiteNo:     "0013", ScnsNo: "006", ScnSseq: "1", MovNo: "30001323",
	}, "2026-08-12", testScheduleTheater())
	if !ok {
		t.Fatal("parseSchedule() rejected the stored CGV fixture")
	}
	showtime := entry.Showtime
	if showtime.ID != contracts.CatalogID(contracts.ProviderCGV, "showtime", showtime.SourceKey) ||
		showtime.AuditoriumID != contracts.CatalogID(
			contracts.ProviderCGV, "auditorium", showtime.AuditoriumSourceKey,
		) || showtime.MovieID != contracts.CatalogID(contracts.ProviderCGV, "movie", showtime.MovieSourceKey) {
		t.Fatalf("canonical identities = showtime %q, auditorium %q", showtime.ID, showtime.AuditoriumID)
	}
	if showtime.ProviderID != contracts.ProviderCGV || showtime.TheaterID != testScheduleTheater().ID ||
		showtime.AuditoriumName != "6관 (Laser)" || !showtime.SoldOut ||
		showtime.StartsAt != "19:45" || showtime.EndsAt != "21:44" {
		t.Fatalf("parsed stored CGV showtime = %+v", showtime)
	}
	if showtime.ObservedAt.IsZero() || showtime.ObservedAt.After(time.Now().Add(time.Second)) {
		t.Fatal("observed time was not populated")
	}
}

func TestParseScheduleAcceptsCompactAvailabilityAndRejectsBadgeOnlyAuditorium(t *testing.T) {
	t.Parallel()
	entry, ok := parseSchedule(rawSchedule{
		Label: "13:30- 16:326/624석", Movie: "오디세이", Group: "IMAX관IMAX LASER 2D",
		SiteNo: "0013", ScnsNo: "018", ScnSseq: "1", MovNo: "30001323",
	}, "2026-08-12", testScheduleTheater())
	if !ok || entry.Showtime.AvailableSeats != 6 || entry.Showtime.Capacity != 624 ||
		entry.AuditoriumName != "IMAX관" || entry.Showtime.AuditoriumID == "" {
		t.Fatalf("compact showtime = %+v, %t", entry, ok)
	}
	if _, ok := parseSchedule(rawSchedule{
		Label: "12:50 - 15:20 38 / 50 석 조조 컬처데이", Movie: "영화", Group: "2D",
		SiteNo: "0013", ScnsNo: "001", ScnSseq: "1", MovNo: "30009999",
	}, "2026-08-12", testScheduleTheater()); ok {
		t.Fatal("badge-only text was accepted as an auditorium")
	}
}

func TestParseSchedulePreservesCGVExtendedHours(t *testing.T) {
	t.Parallel()
	entry, ok := parseSchedule(rawSchedule{
		Label: "24:30 - 27:09 12 / 624 석", Movie: "심야 영화", Group: "IMAX관 IMAX LASER 2D",
		SiteNo: "0013", ScnsNo: "018", ScnSseq: "1", MovNo: "30001323",
	}, "2026-08-21", testScheduleTheater())
	if !ok {
		t.Fatal("extended-hour CGV schedule was rejected")
	}
	if entry.Showtime.Date != "2026-08-21" || entry.Showtime.StartsAt != "24:30" || entry.Showtime.EndsAt != "27:09" {
		t.Fatalf("extended-hour schedule = %+v", entry.Showtime)
	}
}

func TestParseSchedulesFailsClosedWhenAnyCandidateIsRejected(t *testing.T) {
	t.Parallel()
	raw := []rawSchedule{
		{Label: "13:30-16:32 6/624석", Movie: "오디세이", Group: "IMAX관 IMAX LASER 2D"},
		{Label: "CGV가 변경한 표기", Movie: "오디세이", Group: "IMAX관 IMAX LASER 2D"},
	}
	if _, err := parseSchedules(raw, "2026-08-12", testScheduleTheater()); err == nil {
		t.Fatal("partially parsed schedule snapshot was accepted as complete")
	}
}

func TestDateButtonLabelsAcceptTodaySpacingAndOrderVariants(t *testing.T) {
	t.Parallel()
	want := []string{"오늘 12", "오늘12", "12 오늘", "12오늘"}
	got := dateButtonLabels("오늘", "12", true)
	if len(got) != len(want) {
		t.Fatalf("date labels = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("date labels = %v", got)
		}
	}
}

func testScheduleTheater() ScheduleTheater {
	sourceKey := "0013"
	return ScheduleTheater{
		ID:         contracts.CatalogID(contracts.ProviderCGV, "theater", sourceKey),
		ProviderID: contracts.ProviderCGV, SourceKey: sourceKey, Region: "서울", Name: "용산아이파크몰",
	}
}
