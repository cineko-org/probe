package cgv

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maximumProviderClockHour = 47

// ParseShowtimeRange converts CGV's service-day clock into real local time.
// CGV can represent after-midnight times as 2500 or 2709; the provider date
// remains the source identity while the returned time advances to the next day.
func ParseShowtimeRange(
	date string,
	startClock string,
	endClock string,
	location *time.Location,
) (time.Time, time.Time, error) {
	if location == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("showtime location is required")
	}
	canonicalDate, err := canonicalProviderDate(date)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	startHour, startMinutes, err := parseProviderClock(startClock)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start clock: %w", err)
	}
	endHour, endMinutes, err := parseProviderClock(endClock)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid end clock: %w", err)
	}
	base, err := time.ParseInLocation(time.DateOnly, canonicalDate, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	startsAt := base.Add(time.Duration(startHour*60+startMinutes) * time.Minute)
	endsAt := base.Add(time.Duration(endHour*60+endMinutes) * time.Minute)
	if !endsAt.After(startsAt) {
		endsAt = endsAt.Add(24 * time.Hour)
	}
	return startsAt, endsAt, nil
}

func parseProviderClock(raw string) (int, int, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 5 && raw[2] == ':' {
		raw = raw[:2] + raw[3:]
	}
	if len(raw) != 4 {
		return 0, 0, fmt.Errorf("%q is not HHMM or HH:MM", raw)
	}
	hour, err := strconv.Atoi(raw[:2])
	if err != nil {
		return 0, 0, fmt.Errorf("%q has an invalid hour", raw)
	}
	minute, err := strconv.Atoi(raw[2:])
	if err != nil || minute > 59 {
		return 0, 0, fmt.Errorf("%q has an invalid minute", raw)
	}
	if hour > maximumProviderClockHour {
		return 0, 0, fmt.Errorf("%q exceeds the service-day range", raw)
	}
	return hour, minute, nil
}

// providerClockLabel returns the exact clock label rendered by CGV while
// preserving service-day hours after midnight (for example 25:30).
func providerClockLabel(raw string) (string, error) {
	hour, minute, err := parseProviderClock(raw)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), nil
}
