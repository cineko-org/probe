package cgv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type scheduleEntry struct {
	Showtime       ScheduleShowtime
	AuditoriumName string
	ScreenTypes    []string
}

type ScheduleTheater struct {
	ID         string
	ProviderID string
	SourceKey  string
	Region     string
	Name       string
}

type ScheduleShowtime struct {
	ID                  string
	ProviderID          string
	SourceKey           string
	TheaterID           string
	MovieID             string
	MovieSourceKey      string
	MovieTitle          string
	PosterURL           string
	AuditoriumID        string
	AuditoriumSourceKey string
	AuditoriumName      string
	ScreenTypes         []string
	Date                string
	StartsAt            string
	EndsAt              string
	AvailableSeats      int
	Capacity            int
	SoldOut             bool
	ObservedAt          time.Time
	SourceLabel         string
}

type ScheduleCapture struct {
	TargetDate string
	Complete   bool
	Error      string
	Showtimes  []ScheduleShowtime
}

// CaptureSchedules returns a complete, unfiltered snapshot for every requested
// date. Date-level failures stay attached to that date so an unavailable page
// can never be mistaken for evidence that no showtimes existed.
func (adapter *Adapter) CaptureSchedules(
	ctx context.Context,
	theater ScheduleTheater,
	targetDates []string,
) ([]ScheduleCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := adapter.selectCinemaTheater(theater.Region, theater.Name); err != nil {
		return nil, err
	}
	result := make([]ScheduleCapture, 0, len(targetDates))
	for _, targetDate := range targetDates {
		capture := ScheduleCapture{TargetDate: targetDate}
		canonicalDate, err := canonicalProviderDate(targetDate)
		if err != nil {
			capture.Error = err.Error()
			result = append(result, capture)
			continue
		}
		capture.TargetDate = canonicalDate
		if err := adapter.selectDate(canonicalDate); err != nil {
			capture.Error = err.Error()
			result = append(result, capture)
			continue
		}
		entries, err := adapter.extractSchedules(canonicalDate, theater)
		if err != nil {
			capture.Error = err.Error()
			result = append(result, capture)
			continue
		}
		capture.Complete = true
		capture.Showtimes = make([]ScheduleShowtime, 0, len(entries))
		for _, entry := range entries {
			capture.Showtimes = append(capture.Showtimes, entry.Showtime)
		}
		result = append(result, capture)
	}
	return result, nil
}

func (adapter *Adapter) selectCinemaTheater(region, theaterName string) error {
	if err := adapter.navigate(bookingCinemaURL); err != nil {
		return fmt.Errorf("open CGV cinema booking: %w", err)
	}
	clicked, err := adapter.clickButtonPrefix(region + "(")
	if err != nil {
		return err
	}
	if !clicked {
		opened, openErr := adapter.clickButtonExact("자주가는 CGV 목록 수정")
		if openErr != nil {
			return openErr
		}
		if opened {
			if err := adapter.wait(200 * time.Millisecond); err != nil {
				return err
			}
			clicked, err = adapter.clickButtonPrefix(region + "(")
			if err != nil {
				return err
			}
		}
	}
	if !clicked {
		return fmt.Errorf("%w: region button %q not found", ErrUIContractChanged, region)
	}
	if err := adapter.wait(150 * time.Millisecond); err != nil {
		return err
	}
	clicked, err = adapter.clickButtonExact(theaterName)
	if err != nil {
		return err
	}
	if !clicked {
		return fmt.Errorf("%w: theater button %q not found", ErrUIContractChanged, theaterName)
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return err
	}
	if exists, _ := adapter.buttonExists("극장선택"); exists {
		_, _ = adapter.clickButtonExact("극장선택")
	}
	if err := adapter.wait(800 * time.Millisecond); err != nil {
		return err
	}
	adapter.selectedTheater = theaterName
	adapter.selectedRegion = region
	return nil
}

func (adapter *Adapter) selectDate(isoDate string) error {
	parsed, err := time.ParseInLocation(time.DateOnly, isoDate, time.FixedZone("KST", 9*60*60))
	if err != nil {
		return err
	}
	now := time.Now().In(time.FixedZone("KST", 9*60*60))
	labels := dateSelectionLabels(parsed, now)
	for _, label := range labels {
		adapter.resetProviderResponses()
		clicked, err := adapter.clickButtonExact(label)
		if err != nil {
			return err
		}
		if clicked {
			return adapter.wait(500 * time.Millisecond)
		}
	}
	return fmt.Errorf("%w: target date %s", ErrTargetDateUnavailable, isoDate)
}

func dateSelectionLabels(parsed, now time.Time) []string {
	day := parsed.Format("02")
	weekdays := []string{"일", "월", "화", "수", "목", "금", "토"}
	weekday := weekdays[parsed.Weekday()]
	labels := []string{weekday + " " + day, weekday + day, day + " " + weekday, day + weekday}
	if now.In(parsed.Location()).Format(time.DateOnly) == parsed.Format(time.DateOnly) {
		labels = append([]string{"오늘 " + day, "오늘" + day, day + " 오늘", day + "오늘"}, labels...)
	}
	return labels
}

func (adapter *Adapter) extractSchedules(
	date string,
	theater ScheduleTheater,
) ([]scheduleEntry, error) {
	rows, err := adapter.captureScheduleRows()
	if err != nil {
		if errors.Is(err, errScheduleResponseMissing) {
			return nil, fmt.Errorf("%w: %w", ErrUIContractChanged, err)
		}
		return nil, err
	}
	canonicalDate, err := canonicalProviderDate(date)
	if err != nil {
		return nil, err
	}
	entries := make([]scheduleEntry, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if row.Date != canonicalDate {
			return nil, fmt.Errorf(
				"CGV schedule row %q has date %q, expected %q",
				row.Sequence, row.Date, canonicalDate,
			)
		}
		entry, err := scheduleEntryFromProviderRow(row, theater)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[entry.Showtime.ID]; duplicate {
			continue
		}
		seen[entry.Showtime.ID] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, nil
}

func scheduleEntryFromProviderRow(row providerScheduleRow, theater ScheduleTheater) (scheduleEntry, error) {
	theaterSiteNo := strings.TrimSpace(theater.SourceKey)
	if theater.ProviderID != ProviderCGV || !numericIdentifier(theaterSiteNo) ||
		theater.ID != CatalogID(ProviderCGV, "theater", theaterSiteNo) {
		return scheduleEntry{}, fmt.Errorf("%w: CGV theater identity is not canonical", ErrIdentityMismatch)
	}
	if row.SiteNo != theaterSiteNo {
		return scheduleEntry{}, fmt.Errorf("%w: CGV schedule row siteNo %q does not match theater identity", ErrIdentityMismatch, row.SiteNo)
	}
	auditoriumName, screenTypes := parseAuditorium("", row.AuditoriumName)
	if auditoriumName == "" {
		return scheduleEntry{}, fmt.Errorf("CGV schedule row %q has no auditorium display name", row.Sequence)
	}
	movieSource := movieSourceKey(row.MovieNo)
	auditoriumSource := auditoriumSourceKey(row.SiteNo, row.AuditoriumNo)
	showtimeSource := showtimeSourceKey(row.SiteNo, row.Date, row.AuditoriumNo, row.Sequence)
	showtime := ScheduleShowtime{
		ID: providerCatalogID("showtime", showtimeSource), ProviderID: ProviderCGV,
		SourceKey: showtimeSource, TheaterID: theater.ID,
		MovieID: providerCatalogID("movie", movieSource), MovieSourceKey: movieSource,
		MovieTitle: row.MovieTitle, PosterURL: "",
		AuditoriumID: providerCatalogID("auditorium", auditoriumSource), AuditoriumSourceKey: auditoriumSource,
		AuditoriumName: auditoriumName, ScreenTypes: screenTypes,
		Date: row.Date, StartsAt: row.StartClock, EndsAt: row.EndClock,
		AvailableSeats: row.Available, Capacity: row.Capacity,
		SoldOut: row.Available == 0, ObservedAt: time.Now(),
		SourceLabel: strings.Join([]string{row.StartClock, row.EndClock, row.MovieTitle, auditoriumName}, " "),
	}
	return scheduleEntry{Showtime: showtime, AuditoriumName: auditoriumName, ScreenTypes: screenTypes}, nil
}

func parseAuditorium(group, structuredName string) (string, []string) {
	group = normalize(group)
	structuredName = normalize(structuredName)
	types := detectScreenTypes(group + " " + structuredName)
	if structuredName != "" {
		return structuredName, types
	}
	if group == "" || group == "2D" || group == "3D" {
		return "", types
	}
	name := group
	for _, token := range []string{
		"SCREENX DOLBY ATMOS mix 2D", "IMAX LASER 2D", "ULTRA 4DX 2D",
		"SCREENX 2D", "4DX 2D", "DOLBY ATMOS 2D", "2D", "3D",
	} {
		name = strings.TrimSpace(strings.TrimSuffix(name, token))
	}
	return name, types
}

func detectScreenTypes(value string) []string {
	upper := strings.ToUpper(value)
	var types []string
	for _, screenType := range []string{
		"ULTRA 4DX", "SCREENX", "IMAX", "4DX", "DOLBY ATMOS", "PREMIUM",
		"CINE DE CHEF", "CGV아트하우스", "LASER", "2D", "3D",
	} {
		if strings.Contains(upper, strings.ToUpper(screenType)) {
			types = append(types, screenType)
		}
	}
	return types
}

func normalize(value string) string { return strings.Join(strings.Fields(value), " ") }
