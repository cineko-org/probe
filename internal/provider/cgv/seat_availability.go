package cgv

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	seatmappb "github.com/cineko-org/contracts/gen/go/cineko/seatmap"
)

// ErrSeatAvailabilityIncomplete means the provider did not return a complete
// status for every seat in the static layout. It is deliberately not treated
// as an empty availability set: absence is not evidence of sold-out seats.
var ErrSeatAvailabilityIncomplete = errors.New("CGV seat availability is incomplete")

// parseSeatAvailability extracts normalized Cineko seat IDs from the same
// response used for the static layout. Layout and live status remain separate
// resources; the layout is only used to validate that every status maps to a
// known auditorium seat.
func parseSeatAvailability(body []byte, auditoriumID string, layout *seatmappb.Layout) ([]string, error) {
	if layout == nil || strings.TrimSpace(auditoriumID) == "" {
		return nil, fmt.Errorf("%w: layout identity is missing", ErrSeatAvailabilityIncomplete)
	}
	var envelope seatDataEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode CGV seat availability: %w", err)
	}
	if envelope.StatusCode != 0 {
		return nil, fmt.Errorf("CGV seat availability failed: %s", envelope.ResultMsg)
	}
	if len(envelope.Data.Items) == 0 {
		return nil, fmt.Errorf("%w: provider returned no seat areas", ErrSeatAvailabilityIncomplete)
	}

	layoutSeats, err := seatAvailabilityLayoutIndex(layout)
	if err != nil {
		return nil, err
	}
	return providerAvailableSeats(envelope.Data.Items, auditoriumID, layoutSeats)
}

func seatAvailabilityLayoutIndex(layout *seatmappb.Layout) (map[string]struct{}, error) {
	layoutSeats := make(map[string]struct{}, len(layout.GetSeats()))
	for _, seat := range layout.GetSeats() {
		if seat == nil || strings.TrimSpace(seat.GetId()) == "" {
			return nil, fmt.Errorf("%w: static layout contains an invalid seat", ErrSeatAvailabilityIncomplete)
		}
		layoutSeats[seat.GetId()] = struct{}{}
	}
	return layoutSeats, nil
}

func providerAvailableSeats(items []seatDataItem, auditoriumID string, layoutSeats map[string]struct{}) ([]string, error) {
	seen := make(map[string]struct{}, len(layoutSeats))
	available := make([]string, 0, len(layoutSeats))
	for _, item := range items {
		for _, source := range item.Seats {
			label, _, _, err := normalizedSeatLabel(source)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSeatAvailabilityIncomplete, err)
			}
			seatID := SeatID(auditoriumID, label)
			if _, known := layoutSeats[seatID]; !known {
				return nil, fmt.Errorf("%w: provider seat %s is not in the static layout", ErrSeatAvailabilityIncomplete, label)
			}
			if _, duplicate := seen[seatID]; duplicate {
				return nil, fmt.Errorf("%w: duplicate provider seat %s", ErrSeatAvailabilityIncomplete, label)
			}
			seen[seatID] = struct{}{}
			saleYN := strings.ToUpper(strings.TrimSpace(source.SaleYN))
			statusCode := strings.TrimSpace(source.StatusCode)
			if (saleYN != "Y" && saleYN != "N") || !validSeatStatusCode(statusCode) {
				return nil, fmt.Errorf("%w: provider status is missing for seat %s", ErrSeatAvailabilityIncomplete, label)
			}
			if saleYN == "Y" && statusCode == "00" {
				available = append(available, seatID)
			}
		}
	}
	if len(seen) != len(layoutSeats) {
		return nil, fmt.Errorf("%w: provider returned %d of %d layout seats", ErrSeatAvailabilityIncomplete, len(seen), len(layoutSeats))
	}
	sort.Strings(available)
	return available, nil
}

func validSeatStatusCode(value string) bool {
	switch value {
	case "00", "01", "03", "04":
		return true
	default:
		return false
	}
}
