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
	}, "2026-08-12", testScheduleTheater())
	if !ok || entry.Showtime.AvailableSeats != 6 || entry.Showtime.Capacity != 624 ||
		entry.AuditoriumName != "IMAX관" || entry.Showtime.AuditoriumID == "" {
		t.Fatalf("compact showtime = %+v, %t", entry, ok)
	}
	if _, ok := parseSchedule(rawSchedule{
		Label: "12:50 - 15:20 38 / 50 석 조조 컬처데이", Movie: "영화", Group: "2D",
	}, "2026-08-12", testScheduleTheater()); ok {
		t.Fatal("badge-only text was accepted as an auditorium")
	}
}

func testScheduleTheater() ScheduleTheater {
	sourceKey := "서울/용산아이파크몰"
	return ScheduleTheater{
		ID:         contracts.CatalogID(contracts.ProviderCGV, "theater", sourceKey),
		ProviderID: contracts.ProviderCGV, SourceKey: sourceKey, Region: "서울", Name: "용산아이파크몰",
	}
}
