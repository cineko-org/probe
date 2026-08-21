package cgv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	catalogpb "github.com/cineko-org/contracts/gen/go/cineko/catalog"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	seatmappb "github.com/cineko-org/contracts/gen/go/cineko/seatmap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const seatMapResponseTimeout = 8 * time.Second

// CaptureSeatMap visits one exact nonmember seat page and returns only its
// static layout. Live availability is intentionally discarded.
func (adapter *Adapter) CaptureSeatMap(
	ctx context.Context,
	task *observationpb.AssignmentTask,
) (*seatmappb.Snapshot, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := validateSeatMapTask(task); err != nil {
		return nil, err
	}
	seatTask := task.GetSeatMap()
	location, err := time.LoadLocation(seatTask.GetTimeZone())
	if err != nil {
		return nil, fmt.Errorf("load seat-map time zone: %w", err)
	}
	date := seatTask.GetShowtime().GetStartsAt().AsTime().In(location).Format(time.DateOnly)
	if err := adapter.selectCinemaTheater(seatTask.GetTheater().GetRegion(), seatTask.GetTheater().GetName()); err != nil {
		return nil, err
	}
	if err := adapter.selectDate(date); err != nil {
		return nil, err
	}
	entries, err := adapter.extractSchedules(date, ScheduleTheater{
		ID: seatTask.GetTheater().GetId(), ProviderID: seatTask.GetTheater().GetProviderId(),
		SourceKey: seatTask.GetTheater().GetSourceKey(), Region: seatTask.GetTheater().GetRegion(), Name: seatTask.GetTheater().GetName(),
	})
	if err != nil {
		return nil, err
	}
	showtime, err := exactSeatMapShowtime(entries, seatTask.GetShowtime())
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
	layout, err := parseSeatMapLayout(captured.body, seatTask.GetAuditorium().GetId())
	if err != nil {
		return nil, err
	}
	snapshot := &seatmappb.Snapshot{}
	hash, err := layoutHash(layout)
	if err != nil {
		return nil, fmt.Errorf("hash seat-map layout: %w", err)
	}
	snapshot.SetId(SeatMapVersionID(seatTask.GetAuditorium().GetId(), hash))
	snapshot.SetAuditoriumId(seatTask.GetAuditorium().GetId())
	snapshot.SetLayoutHash(hash)
	capacity, err := seatCountAsInt32(len(layout.GetSeats()))
	if err != nil {
		return nil, errors.New("seat-map layout contains too many seats")
	}
	snapshot.SetCapacity(capacity)
	snapshot.SetLayout(layout)
	snapshot.SetObservedAt(timestamppb.New(time.Now().UTC()))
	return snapshot, nil
}

func seatCountAsInt32(value int) (int32, error) {
	if value < 0 || value > math.MaxInt32 {
		return 0, errors.New("seat count is outside int32 range")
	}
	return int32(value), nil //nolint:gosec // the range check above bounds this conversion
}

func layoutHash(layout *seatmappb.Layout) (string, error) {
	canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(layout)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validateSeatMapTask(task *observationpb.AssignmentTask) error {
	seatTask := task.GetSeatMap()
	if seatTask == nil || seatTask.GetAuditorium() == nil || seatTask.GetShowtime() == nil ||
		seatTask.GetTheater() == nil || seatTask.GetShowtime().GetStartsAt() == nil {
		return errors.New("seat-map assignment target is incomplete")
	}
	if err := validateSeatMapTheater(seatTask.GetTheater()); err != nil {
		return err
	}
	if err := validateSeatMapAuditorium(seatTask); err != nil {
		return err
	}
	if err := validateSeatMapShowtime(seatTask.GetShowtime()); err != nil {
		return err
	}
	if strings.TrimSpace(seatTask.GetTimeZone()) == "" {
		return errors.New("seat-map assignment time zone is required")
	}
	return nil
}

func validateSeatMapTheater(theater *catalogpb.Theater) error {
	if theater.GetProviderId() != ProviderCGV || theater.GetSourceKey() == "" ||
		theater.GetId() != CatalogID(ProviderCGV, "theater", theater.GetSourceKey()) {
		return errors.New("seat-map theater identity is not canonical")
	}
	return nil
}

func validateSeatMapAuditorium(task *observationpb.SeatMapTask) error {
	if task.GetAuditorium().GetTheaterId() != task.GetTheater().GetId() || task.GetShowtime().GetAuditorium().GetId() != task.GetAuditorium().GetId() ||
		task.GetAuditorium().GetId() != CatalogID(ProviderCGV, "auditorium", task.GetAuditorium().GetSourceKey()) {
		return errors.New("seat-map auditorium identity is not canonical")
	}
	return nil
}

func validateSeatMapShowtime(showtime *catalogpb.Showtime) error {
	if showtime.GetProviderId() != ProviderCGV || showtime.GetSourceKey() == "" ||
		showtime.GetId() != CatalogID(ProviderCGV, "showtime", showtime.GetSourceKey()) ||
		showtime.GetMovie().GetId() == "" || showtime.GetStartsAt() == nil || showtime.GetEndsAt() == nil || !showtime.GetEndsAt().AsTime().After(showtime.GetStartsAt().AsTime()) {
		return errors.New("seat-map showtime identity is not canonical")
	}
	return nil
}

func exactSeatMapShowtime(entries []scheduleEntry, command *catalogpb.Showtime) (ScheduleShowtime, error) {
	var matches []ScheduleShowtime
	for _, entry := range entries {
		if entry.Showtime.SourceKey == command.GetSourceKey() {
			matches = append(matches, entry.Showtime)
		}
	}
	if len(matches) != 1 {
		return ScheduleShowtime{}, fmt.Errorf("%w: expected one provider row for %s, got %d", ErrUIContractChanged, command.GetSourceKey(), len(matches))
	}
	match := matches[0]
	if match.MovieID != command.GetMovie().GetId() || match.AuditoriumID != command.GetAuditorium().GetId() {
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
