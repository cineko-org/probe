package cgv

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

var schedulePattern = regexp.MustCompile(
	`^(\d{2}:\d{2})\s*-\s*(\d{2}:\d{2})\s*(?:(\d+)\s*/\s*(\d+)\s*석|(매진|예매종료))(?:\s*(.*))?$`,
)

type rawSchedule struct {
	Label      string `json:"label"`
	Movie      string `json:"movie"`
	PosterURL  string `json:"posterUrl"`
	Group      string `json:"group"`
	Auditorium string `json:"auditorium"`
	Disabled   bool   `json:"disabled"`
	SiteNo     string `json:"siteNo"`
	ScnsNo     string `json:"scnsNo"`
	ScnYmd     string `json:"scnYmd"`
	ScnSseq    string `json:"scnSseq"`
	MovNo      string `json:"movNo"`
	MovfNo     string `json:"movfNo"`
	ProdNo     string `json:"prodNo"`
}

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
		if err := adapter.selectDate(targetDate); err != nil {
			capture.Error = err.Error()
			result = append(result, capture)
			continue
		}
		entries, err := adapter.extractSchedules(targetDate, theater)
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
		sort.Slice(capture.Showtimes, func(i, j int) bool {
			return capture.Showtimes[i].ID < capture.Showtimes[j].ID
		})
		result = append(result, capture)
	}
	return result, nil
}

func (adapter *Adapter) selectCinemaTheater(region, theaterName string) error {
	adapter.clearProviderResponse("schedules")
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
	parsed, err := time.ParseInLocation("2006-01-02", isoDate, time.FixedZone("KST", 9*60*60))
	if err != nil {
		return err
	}
	day := parsed.Format("02")
	weekdays := []string{"일", "월", "화", "수", "목", "금", "토"}
	labels := dateButtonLabels(weekdays[parsed.Weekday()], day, false)
	now := time.Now().In(time.FixedZone("KST", 9*60*60))
	if now.Format("2006-01-02") == isoDate {
		labels = append(dateButtonLabels("오늘", day, true), labels...)
	}
	for _, label := range labels {
		adapter.clearProviderResponse("schedules")
		clicked, err := adapter.clickButtonExact(label)
		if err != nil {
			return err
		}
		if clicked {
			if err := adapter.wait(500 * time.Millisecond); err != nil {
				return err
			}
			return adapter.waitForProviderResponse("schedules", 2*time.Second)
		}
	}
	return fmt.Errorf("%w: target date %s is not selectable", ErrUIContractChanged, isoDate)
}

func dateButtonLabels(prefix, day string, allowReverse bool) []string {
	labels := []string{prefix + " " + day, prefix + day}
	if allowReverse {
		labels = append(labels, day+" "+prefix, day+prefix)
	}
	return labels
}

func (adapter *Adapter) extractSchedules(
	date string,
	theater ScheduleTheater,
) ([]scheduleEntry, error) {
	if payload := adapter.providerResponse("schedules"); len(payload) > 0 {
		entries, err := parseCGVScheduleResponse(payload, date, theater)
		if err != nil {
			return nil, fmt.Errorf("parse CGV structured schedules: %w", err)
		}
		return entries, nil
	}
	const expression = `(() => {
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const previous = (element, selector) => {
			let current = element;
			while (current) {
				let sibling = current.previousElementSibling;
				while (sibling) {
					if (sibling.matches && sibling.matches(selector)) return sibling;
					const nested = sibling.querySelectorAll ? window.__cinekoQueryAll(selector, sibling) : [];
					if (nested.length) return nested[nested.length - 1];
					sibling = sibling.previousElementSibling;
				}
				current = current.parentElement;
			}
			return null;
		};
		return window.__cinekoQueryAll('button').map(button => {
			const label = normalize(button.innerText || button.textContent);
			if (!/^\d{2}:\d{2}-/.test(label)) return null;
			const group = previous(button, 'h3');
			const movieHeading = previous(button, 'h2');
			const poster = movieHeading && window.__cinekoQuery('img[alt*="포스터"]', movieHeading);
			const auditorium = window.__cinekoQuery('[class*="_theater__"]', button);
			return {
				label,
				movie: poster ? normalize(poster.getAttribute('alt')).replace(/\s*포스터$/, '') : normalize(movieHeading && movieHeading.innerText),
				posterUrl: poster ? normalize(poster.currentSrc || poster.getAttribute('src')) : '',
				group: normalize(group && group.innerText),
				auditorium: normalize(auditorium && auditorium.innerText),
				disabled: !!button.disabled
			};
		}).filter(Boolean);
	})()`
	var raw []rawSchedule
	if err := adapter.evaluate(expression, &raw); err != nil {
		return nil, fmt.Errorf("extract CGV schedules: %w", err)
	}
	return parseSchedules(raw, date, theater)
}

func parseSchedules(raw []rawSchedule, date string, theater ScheduleTheater) ([]scheduleEntry, error) {
	entries := make([]scheduleEntry, 0, len(raw))
	for _, item := range raw {
		entry, ok := parseSchedule(item, date, theater)
		if !ok {
			return nil, fmt.Errorf(
				"%w: rejected one of %d schedule candidates", ErrUIContractChanged, len(raw),
			)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func parseSchedule(item rawSchedule, date string, theater ScheduleTheater) (scheduleEntry, bool) {
	canonicalDate := canonicalCGVDate(date)
	if canonicalDate == "" {
		return scheduleEntry{}, false
	}
	match := schedulePattern.FindStringSubmatch(normalize(item.Label))
	if match == nil {
		return scheduleEntry{}, false
	}
	available, capacity := 0, 0
	if match[3] != "" {
		_, _ = fmt.Sscanf(match[3], "%d", &available)
		_, _ = fmt.Sscanf(match[4], "%d", &capacity)
	}
	auditoriumName, screenTypes := parseAuditorium(item.Group, item.Auditorium)
	if auditoriumName == "" {
		return scheduleEntry{}, false
	}
	if !validCGVProviderKeys(item.SiteNo, item.MovNo, item.ScnsNo, canonicalDate, item.ScnSseq) {
		return scheduleEntry{}, false
	}
	if item.SiteNo != strings.TrimSpace(theater.SourceKey) {
		return scheduleEntry{}, false
	}
	movieSourceKey := cgvMovieSourceKey(item.MovNo)
	auditoriumSourceKey := cgvAuditoriumSourceKey(item.SiteNo, item.ScnsNo)
	showtimeSourceKey := cgvShowtimeSourceKey(item.SiteNo, canonicalDate, item.ScnsNo, item.ScnSseq)
	showtime := ScheduleShowtime{
		ID:         contracts.CatalogID(contracts.ProviderCGV, "showtime", showtimeSourceKey),
		ProviderID: contracts.ProviderCGV, SourceKey: showtimeSourceKey, TheaterID: theater.ID,
		MovieID:        contracts.CatalogID(contracts.ProviderCGV, "movie", movieSourceKey),
		MovieSourceKey: movieSourceKey, MovieTitle: item.Movie, PosterURL: item.PosterURL,
		AuditoriumID:        contracts.CatalogID(contracts.ProviderCGV, "auditorium", auditoriumSourceKey),
		AuditoriumSourceKey: auditoriumSourceKey,
		AuditoriumName:      auditoriumName, ScreenTypes: screenTypes,
		Date: canonicalDate, StartsAt: match[1], EndsAt: match[2],
		AvailableSeats: available, Capacity: capacity,
		SoldOut: item.Disabled || match[5] != "", ObservedAt: time.Now(),
		SourceLabel: normalize(item.Label),
	}
	return scheduleEntry{Showtime: showtime, AuditoriumName: auditoriumName, ScreenTypes: screenTypes}, true
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
