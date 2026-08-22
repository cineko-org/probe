package cgv

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
)

// TheaterSiteNo returns the canonical CGV site number carried by a catalog
// entity. Display text is intentionally not considered an identity.
func TheaterSiteNo(theater *catalogpb.Theater) (string, bool) {
	if theater == nil || theater.GetIdentity() == nil || theater.GetIdentity().GetCgv() == nil {
		return "", false
	}
	value := strings.TrimSpace(theater.GetIdentity().GetCgv().GetSiteNo())
	return value, providerSiteIdentifier(value)
}

// AuditoriumIdentityValues returns the typed CGV site/screen tuple.
func AuditoriumIdentityValues(auditorium *catalogpb.Auditorium) (string, string, bool) {
	if auditorium == nil || auditorium.GetIdentity() == nil || auditorium.GetIdentity().GetCgv() == nil {
		return "", "", false
	}
	identity := auditorium.GetIdentity().GetCgv()
	siteNo, screenNo := strings.TrimSpace(identity.GetSiteNo()), strings.TrimSpace(identity.GetScreenNo())
	return siteNo, screenNo, providerSiteIdentifier(siteNo) && numericIdentifier(screenNo)
}

// ShowtimeIdentityValues returns the typed CGV identity tuple. The schedule
// date is kept in provider-date form for matching the CGV response.
func ShowtimeIdentityValues(showtime *catalogpb.Showtime) (string, string, string, string, bool) {
	if showtime == nil || showtime.GetIdentity() == nil || showtime.GetIdentity().GetCgv() == nil {
		return "", "", "", "", false
	}
	identity := showtime.GetIdentity().GetCgv()
	siteNo, screenNo, sequence := strings.TrimSpace(identity.GetSiteNo()), strings.TrimSpace(identity.GetScreenNo()), strings.TrimSpace(identity.GetSequence())
	date := localDateValue(identity.GetScheduleDate())
	return siteNo, date, screenNo, sequence, providerSiteIdentifier(siteNo) && date != "" && numericIdentifier(screenNo) && numericIdentifier(sequence)
}

func NewTheaterIdentity(siteNo string) *catalogpb.TheaterIdentity {
	identity := &catalogpb.CgvTheaterIdentity{}
	identity.SetSiteNo(strings.TrimSpace(siteNo))
	result := &catalogpb.TheaterIdentity{}
	result.SetCgv(identity)
	return result
}

func NewMovieIdentity(movieNo string) *catalogpb.MovieIdentity {
	identity := &catalogpb.CgvMovieIdentity{}
	identity.SetMovieNo(strings.TrimSpace(movieNo))
	result := &catalogpb.MovieIdentity{}
	result.SetCgv(identity)
	return result
}

func NewAuditoriumIdentity(siteNo, screenNo string) *catalogpb.AuditoriumIdentity {
	identity := &catalogpb.CgvAuditoriumIdentity{}
	identity.SetSiteNo(strings.TrimSpace(siteNo))
	identity.SetScreenNo(strings.TrimSpace(screenNo))
	result := &catalogpb.AuditoriumIdentity{}
	result.SetCgv(identity)
	return result
}

func NewShowtimeIdentity(siteNo, date, screenNo, sequence string) (*catalogpb.ShowtimeIdentity, error) {
	scheduleDate, err := contractLocalDate(date)
	if err != nil {
		return nil, err
	}
	identity := &catalogpb.CgvShowtimeIdentity{}
	identity.SetSiteNo(strings.TrimSpace(siteNo))
	identity.SetScheduleDate(scheduleDate)
	identity.SetScreenNo(strings.TrimSpace(screenNo))
	identity.SetSequence(strings.TrimSpace(sequence))
	result := &catalogpb.ShowtimeIdentity{}
	result.SetCgv(identity)
	return result, nil
}

func localDateValue(value *commonpb.LocalDate) string {
	if value == nil || value.GetYear() < 1 || value.GetMonth() < 1 || value.GetMonth() > 12 || value.GetDay() < 1 || value.GetDay() > 31 {
		return ""
	}
	date := time.Date(int(value.GetYear()), time.Month(value.GetMonth()), int(value.GetDay()), 0, 0, 0, 0, time.UTC)
	if date.Year() != int(value.GetYear()) || int(date.Month()) != int(value.GetMonth()) || date.Day() != int(value.GetDay()) {
		return ""
	}
	return date.Format(time.DateOnly)
}

func contractLocalDate(value string) (*commonpb.LocalDate, error) {
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid local date %q: %w", value, err)
	}
	result := &commonpb.LocalDate{}
	result.SetYear(int32(parsed.Year()))   //nolint:gosec // time.Parse(time.DateOnly) bounds the year.
	result.SetMonth(int32(parsed.Month())) //nolint:gosec // time.Parse(time.DateOnly) bounds the month.
	result.SetDay(int32(parsed.Day()))     //nolint:gosec // time.Parse(time.DateOnly) bounds the day.
	return result, nil
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func providerSiteIdentifier(value string) bool {
	return value != "" && utf8.RuneCountInString(value) <= 64
}
