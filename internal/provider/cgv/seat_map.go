package cgv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

const seatMapResponseTimeout = 8 * time.Second

// CaptureSeatMap visits one exact nonmember seat page and returns only its
// static layout. Live availability is intentionally discarded.
func (adapter *Adapter) CaptureSeatMap(
	ctx context.Context,
	task contracts.AssignmentTask,
) (*contracts.SeatMapVersion, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := validateSeatMapTask(task); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(task.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load seat-map time zone: %w", err)
	}
	date := task.Showtime.StartsAt.In(location).Format(time.DateOnly)
	if err := adapter.selectCinemaTheater(task.Theater.Region, task.Theater.Name); err != nil {
		return nil, err
	}
	if err := adapter.selectDate(date); err != nil {
		return nil, err
	}
	entries, err := adapter.extractSchedules(date, ScheduleTheater{
		ID: task.Theater.ID, ProviderID: task.Theater.ProviderID,
		SourceKey: task.Theater.SourceKey, Region: task.Theater.Region, Name: task.Theater.Name,
	})
	if err != nil {
		return nil, err
	}
	showtime, err := exactSeatMapShowtime(entries, *task.Showtime)
	if err != nil {
		return nil, err
	}
	if err := adapter.openNonmemberSeatPage(showtime); err != nil {
		return nil, err
	}
	adapter.resetProviderResponses()
	clicked, err := adapter.clickSeatMapRefresh()
	if err != nil {
		return nil, err
	}
	if !clicked {
		return nil, fmt.Errorf("%w: seat refresh button not found", ErrUIContractChanged)
	}
	captured, err := adapter.waitForProviderResponse(ctx, seatMapResponsePath, seatMapResponseTimeout)
	if err != nil {
		return nil, err
	}
	if captured.err != nil {
		return nil, adapter.handleProviderFailure(captured.err)
	}
	layout, err := parseSeatMapLayout(captured.body, task.Auditorium.ID)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(layout)
	if err != nil {
		return nil, fmt.Errorf("encode seat-map layout: %w", err)
	}
	return &contracts.SeatMapVersion{
		AuditoriumID: task.Auditorium.ID, Capacity: len(layout.Seats),
		Layout: encoded, ObservedAt: time.Now().UTC(),
	}, nil
}

func validateSeatMapTask(task contracts.AssignmentTask) error {
	if task.Kind != contracts.CapabilityCGVSeatMapCapture || task.Auditorium == nil || task.Showtime == nil {
		return errors.New("seat-map assignment target is incomplete")
	}
	if err := validateSeatMapTheater(task.Theater); err != nil {
		return err
	}
	if err := validateSeatMapAuditorium(task); err != nil {
		return err
	}
	if err := validateSeatMapShowtime(*task.Showtime); err != nil {
		return err
	}
	if strings.TrimSpace(task.TimeZone) == "" {
		return errors.New("seat-map assignment time zone is required")
	}
	return nil
}

func validateSeatMapTheater(theater contracts.Theater) error {
	if theater.ProviderID != contracts.ProviderCGV || theater.SourceKey == "" ||
		theater.ID != contracts.CatalogID(contracts.ProviderCGV, "theater", theater.SourceKey) {
		return errors.New("seat-map theater identity is not canonical")
	}
	return nil
}

func validateSeatMapAuditorium(task contracts.AssignmentTask) error {
	if task.Auditorium.TheaterID != task.Theater.ID || task.Showtime.Auditorium.ID != task.Auditorium.ID ||
		task.Auditorium.ID != contracts.CatalogID(contracts.ProviderCGV, "auditorium", task.Auditorium.SourceKey) {
		return errors.New("seat-map auditorium identity is not canonical")
	}
	return nil
}

func validateSeatMapShowtime(showtime contracts.Showtime) error {
	if showtime.ProviderID != contracts.ProviderCGV || showtime.SourceKey == "" ||
		showtime.ID != contracts.CatalogID(contracts.ProviderCGV, "showtime", showtime.SourceKey) ||
		showtime.Movie.ID == "" || !showtime.EndsAt.After(showtime.StartsAt) {
		return errors.New("seat-map showtime identity is not canonical")
	}
	return nil
}

func exactSeatMapShowtime(entries []scheduleEntry, command contracts.Showtime) (ScheduleShowtime, error) {
	var matches []ScheduleShowtime
	for _, entry := range entries {
		if entry.Showtime.SourceKey == command.SourceKey {
			matches = append(matches, entry.Showtime)
		}
	}
	if len(matches) != 1 {
		return ScheduleShowtime{}, fmt.Errorf("%w: expected one provider row for %s, got %d", ErrUIContractChanged, command.SourceKey, len(matches))
	}
	match := matches[0]
	if match.MovieID != command.Movie.ID || match.AuditoriumID != command.Auditorium.ID {
		return ScheduleShowtime{}, fmt.Errorf("%w: provider showtime tuple changed", ErrUIContractChanged)
	}
	return match, nil
}

func (adapter *Adapter) openNonmemberSeatPage(showtime ScheduleShowtime) error {
	clicked, err := adapter.clickExactSeatMapShowtime(showtime)
	if err != nil {
		return err
	}
	if !clicked {
		return fmt.Errorf("%w: exact showtime %s was not found", ErrUIContractChanged, showtime.ID)
	}
	if err := adapter.wait(800 * time.Millisecond); err != nil {
		return err
	}
	nonmember, err := adapter.clickButtonExact("비회원 예매")
	if err != nil {
		return err
	}
	if nonmember {
		if err := adapter.wait(500 * time.Millisecond); err != nil {
			return err
		}
	}
	loginRequired, err := adapter.pageContains("CGV 회원 로그인이 필요한 서비스")
	if err != nil {
		return err
	}
	if loginRequired {
		return ErrAuthenticationRequired
	}
	return nil
}

func (adapter *Adapter) clickExactSeatMapShowtime(showtime ScheduleShowtime) (bool, error) {
	seatTotals := fmt.Sprintf("%d/%d석", showtime.AvailableSeats, showtime.Capacity)
	expression := fmt.Sprintf(`(() => {
		const expectedRow = [%s, %s];
		const expectedButton = [%s, %s];
		const expectedSeats = %s;
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const compact = value => normalize(value).replace(/\s+/g, '');
		const scopeText = element => {
			const values = [];
			let current = element;
			for (let depth = 0; current && depth < 5; depth += 1, current = current.parentElement) {
				values.push(normalize(current.innerText || current.textContent));
			}
			return values.join(' ');
		};
		const matches = window.__cinekoQueryAll('button').filter(candidate => {
			if (candidate.disabled) return false;
			const buttonText = normalize(candidate.innerText || candidate.textContent);
			const renderedText = scopeText(candidate);
			return expectedRow.every(value => renderedText.includes(normalize(value))) &&
				expectedButton.every(value => buttonText.includes(normalize(value))) &&
				compact(buttonText).includes(compact(expectedSeats));
		});
		if (matches.length !== 1) return {count: matches.length, clicked: false};
		matches[0].scrollIntoView({block: 'center'});
		matches[0].click();
		return {count: 1, clicked: true};
	})()`, jsString(showtime.MovieTitle), jsString(showtime.AuditoriumName),
		jsString(showtime.StartsAt), jsString(showtime.EndsAt), jsString(seatTotals))
	var result struct {
		Count   int  `json:"count"`
		Clicked bool `json:"clicked"`
	}
	if err := adapter.evaluate(expression, &result); err != nil {
		return false, err
	}
	if result.Count > 1 {
		return false, fmt.Errorf("%w: showtime display is ambiguous for %s", ErrUIContractChanged, showtime.SourceKey)
	}
	return result.Clicked, nil
}

func (adapter *Adapter) clickSeatMapRefresh() (bool, error) {
	const expression = `(() => {
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const button = window.__cinekoQueryAll('button').find(item => {
			const label = normalize(item.getAttribute('aria-label') || item.title || item.innerText || item.textContent);
			return !item.disabled && (label === '새로고침' || label === 'Refresh');
		});
		if (!button) return false;
		button.click();
		return true;
	})()`
	var clicked bool
	err := adapter.evaluate(expression, &clicked)
	return clicked, err
}

func (adapter *Adapter) pageContains(text string) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const body = document.body && (document.body.innerText || document.body.textContent) || '';
		return body.includes(%s);
	})()`, jsString(text))
	var contains bool
	err := adapter.evaluate(expression, &contains)
	return contains, err
}

func (adapter *Adapter) waitForProviderResponse(
	ctx context.Context,
	path string,
	timeout time.Duration,
) (capturedProviderResponse, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		captures := adapter.takeProviderResponses(path)
		if len(captures) > 1 {
			return capturedProviderResponse{}, fmt.Errorf("%w: multiple seat-map responses", ErrUIContractChanged)
		}
		if len(captures) == 1 {
			return captures[0], nil
		}
		select {
		case <-ctx.Done():
			return capturedProviderResponse{}, ctx.Err()
		case <-adapter.ctx.Done():
			return capturedProviderResponse{}, adapter.ctx.Err()
		case <-deadline.C:
			return capturedProviderResponse{}, errors.New("timed out waiting for CGV seat-map response")
		case <-ticker.C:
		}
	}
}
