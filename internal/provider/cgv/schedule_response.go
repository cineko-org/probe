package cgv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	// CGV exposes this response from the booking page. Keep the paths exact so
	// a changed page cannot silently turn a display scrape into an identity.
	scheduleResponsePath         = "/api/v1/booking/searchMovScnInfo"
	theaterCatalogResponsePath   = "/api/v1/content/site/searchAllRegionAndSite"
	seatMapResponsePath          = "/api/v1/booking/searchIfSeatData"
	maxScheduleResponseBytes     = 8 << 20
	maxCapturedProviderResponses = 32
)

var errScheduleResponseMissing = errors.New("CGV schedule response was not captured")

type providerScheduleRow struct {
	SiteNo         string
	SiteName       string
	MovieNo        string
	MovieFileNo    string
	MovieTitle     string
	AuditoriumNo   string
	AuditoriumName string
	Date           string
	Sequence       string
	ProductNo      string
	StartClock     string
	EndClock       string
	Available      int
	Capacity       int
}

// parseScheduleResponse validates the provider envelope and returns only rows
// whose identity and scheduling fields came from the response itself.
func parseScheduleResponse(payload []byte) ([]providerScheduleRow, error) {
	if len(payload) == 0 || len(payload) > maxScheduleResponseBytes {
		return nil, fmt.Errorf("invalid CGV schedule response size %d", len(payload))
	}
	var envelope struct {
		StatusCode    json.RawMessage `json:"statusCode"`
		StatusMessage string          `json:"statusMessage"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode CGV schedule response: %w", err)
	}
	statusCode, err := requiredInteger(envelope.StatusCode, "statusCode")
	if err != nil {
		return nil, err
	}
	if statusCode != 0 {
		message := strings.TrimSpace(envelope.StatusMessage)
		if message == "" {
			message = "provider returned a non-zero status"
		}
		return nil, fmt.Errorf("CGV schedule response status %d: %s", statusCode, message)
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil, errors.New("CGV schedule response data is missing")
	}
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rawRows); err != nil {
		return nil, fmt.Errorf("decode CGV schedule rows: %w", err)
	}
	rows := make([]providerScheduleRow, 0, len(rawRows))
	for index, rawRow := range rawRows {
		row, err := parseScheduleRow(rawRow)
		if err != nil {
			return nil, fmt.Errorf("CGV schedule row %d: %w", index, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseScheduleRow(raw map[string]json.RawMessage) (providerScheduleRow, error) {
	siteNo, err := requiredString(raw, "siteNo")
	if err != nil {
		return providerScheduleRow{}, err
	}
	movieNo, err := requiredString(raw, "movNo")
	if err != nil {
		return providerScheduleRow{}, err
	}
	auditoriumNo, err := requiredString(raw, "scnsNo")
	if err != nil {
		return providerScheduleRow{}, err
	}
	dateRaw, err := requiredString(raw, "scnYmd")
	if err != nil {
		return providerScheduleRow{}, err
	}
	date, err := canonicalProviderDate(dateRaw)
	if err != nil {
		return providerScheduleRow{}, err
	}
	sequence, err := requiredString(raw, "scnSseq")
	if err != nil {
		return providerScheduleRow{}, err
	}
	startClock, err := requiredString(raw, "scnsrtTm")
	if err != nil {
		return providerScheduleRow{}, err
	}
	endClock, err := requiredString(raw, "scnendTm")
	if err != nil {
		return providerScheduleRow{}, err
	}
	if _, _, err := parseProviderClock(startClock); err != nil {
		return providerScheduleRow{}, fmt.Errorf("invalid scnsrtTm: %w", err)
	}
	if _, _, err := parseProviderClock(endClock); err != nil {
		return providerScheduleRow{}, fmt.Errorf("invalid scnendTm: %w", err)
	}
	available, err := optionalInteger(raw, "frSeatCnt")
	if err != nil {
		return providerScheduleRow{}, err
	}
	capacity, err := optionalInteger(raw, "stcnt")
	if err != nil {
		return providerScheduleRow{}, err
	}
	return providerScheduleRow{
		SiteNo: siteNo, SiteName: optionalProviderString(raw, "siteNm"),
		MovieNo: movieNo, MovieFileNo: optionalProviderString(raw, "movfNo"), MovieTitle: optionalProviderString(raw, "movNm"),
		AuditoriumNo: auditoriumNo, AuditoriumName: optionalProviderString(raw, "scnsNm"),
		Date: date, Sequence: sequence, ProductNo: optionalProviderString(raw, "prodNo"),
		StartClock: startClock, EndClock: endClock, Available: available, Capacity: capacity,
	}, nil
}

func requiredString(raw map[string]json.RawMessage, field string) (string, error) {
	value, ok := raw[field]
	if !ok {
		return "", fmt.Errorf("required provider field %q is missing", field)
	}
	return stringValue(value, field)
}

func optionalProviderString(raw map[string]json.RawMessage, field string) string {
	value, ok := raw[field]
	if !ok {
		return ""
	}
	decoded, _ := stringValue(value, field)
	return decoded
}

func stringValue(raw json.RawMessage, field string) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("provider field %q is empty", field)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return "", fmt.Errorf("provider field %q is empty", field)
		}
		return text, nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil && strings.TrimSpace(number.String()) != "" {
		return strings.TrimSpace(number.String()), nil
	}
	return "", fmt.Errorf("provider field %q is not a string or number", field)
}

func requiredInteger(raw json.RawMessage, field string) (int, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, fmt.Errorf("required provider field %q is missing", field)
	}
	value, err := stringValue(raw, field)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("provider field %q is not an integer: %w", field, err)
	}
	return parsed, nil
}

func optionalInteger(raw map[string]json.RawMessage, field string) (int, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return 0, nil
	}
	return requiredInteger(value, field)
}

func scheduleResponseURL(rawURL string) bool {
	path, ok := providerResponsePath(rawURL)
	return ok && path == scheduleResponsePath
}

func providerResponsePath(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "cgv.co.kr" && host != "www.cgv.co.kr" {
		return "", false
	}
	switch parsed.Path {
	case scheduleResponsePath, theaterCatalogResponsePath, seatMapResponsePath:
		return parsed.Path, true
	default:
		return "", false
	}
}
