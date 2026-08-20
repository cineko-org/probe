package cgv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type providerTheaterRow struct {
	SiteNo   string
	SiteName string
	Region   string
}

func parseTheaterCatalogResponse(payload []byte) ([]providerTheaterRow, error) {
	if len(payload) == 0 || len(payload) > maxScheduleResponseBytes {
		return nil, fmt.Errorf("invalid CGV theater response size %d", len(payload))
	}
	var envelope struct {
		StatusCode    json.RawMessage `json:"statusCode"`
		StatusMessage string          `json:"statusMessage"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode CGV theater response: %w", err)
	}
	statusCode, err := requiredInteger(envelope.StatusCode, "statusCode")
	if err != nil {
		return nil, err
	}
	if statusCode != 0 {
		return nil, fmt.Errorf("CGV theater response status %d: %s", statusCode, envelope.StatusMessage)
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil, fmt.Errorf("CGV theater response data is missing")
	}
	var rawRows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rawRows); err != nil {
		return nil, fmt.Errorf("decode CGV theater rows: %w", err)
	}
	rows := make([]providerTheaterRow, 0, len(rawRows))
	for index, rawRow := range rawRows {
		siteNo, err := requiredString(rawRow, "siteNo")
		if err != nil {
			return nil, fmt.Errorf("CGV theater row %d: %w", index, err)
		}
		siteName, err := requiredString(rawRow, "siteNm")
		if err != nil {
			return nil, fmt.Errorf("CGV theater row %d: %w", index, err)
		}
		rows = append(rows, providerTheaterRow{
			SiteNo: siteNo, SiteName: siteName,
			Region: optionalProviderString(rawRow, "regnGrpNm"),
		})
	}
	return rows, nil
}

func (adapter *Adapter) captureTheaterRows() ([]providerTheaterRow, error) {
	captures := adapter.takeProviderResponses(theaterCatalogResponsePath)
	if len(captures) == 0 {
		return nil, errTheaterResponseMissing
	}
	var rows []providerTheaterRow
	seen := make(map[string]struct{})
	for _, captured := range captures {
		if captured.err != nil {
			return nil, adapter.handleProviderFailure(captured.err)
		}
		parsed, err := parseTheaterCatalogResponse(captured.body)
		if err != nil {
			return nil, err
		}
		for _, row := range parsed {
			if _, exists := seen[row.SiteNo]; exists {
				continue
			}
			seen[row.SiteNo] = struct{}{}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

var errTheaterResponseMissing = errors.New("CGV theater response was not captured")
