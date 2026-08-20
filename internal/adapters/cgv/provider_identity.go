package cgv

// This file is deliberately independent of the CGV DOM. CGV exposes the
// identifiers used by the booking APIs in JSON responses; display labels are
// mutable presentation data and must never be used as catalog identity.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

var cgvRegionNames = map[string]string{
	"01": "서울", "02": "경기", "03": "인천", "04": "강원", "05": "대전",
	"06": "충청", "07": "대구", "08": "부산", "09": "경상", "10": "광주",
	"11": "전라", "12": "제주",
}

type cgvScheduleRecord struct {
	SiteNo       string
	ScnsNo       string
	ScnYmd       string
	ScnSseq      string
	ProdNo       string
	MovNo        string
	MovfNo       string
	MovieTitle   string
	MovieName    string
	Auditorium   string
	AuditoriumNo string
	StartsAt     string
	EndsAt       string
	Available    int
	Capacity     int
	SoldOut      bool
}

// cgvString reads both JSON strings and JSON numbers. CGV has returned both
// representations for sequence and identifier fields over time.
func cgvString(value json.RawMessage) string {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(value, &number) == nil {
		return strings.TrimSpace(number.String())
	}
	return ""
}

func cgvField(object map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		if value := cgvString(object[name]); value != "" {
			return value
		}
	}
	return ""
}

func cgvInt(object map[string]json.RawMessage, names ...string) int {
	value := cgvField(object, names...)
	parsed, _ := strconv.Atoi(strings.TrimSpace(value))
	return parsed
}

func cgvArray(payload []byte, keys ...string) ([]map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("decode CGV provider response: %w", err)
	}
	var candidates []json.RawMessage
	if data := root["data"]; len(data) != 0 {
		candidates = append(candidates, data)
	}
	candidates = append(candidates, payload)
	for _, candidate := range candidates {
		var objects []map[string]json.RawMessage
		if json.Unmarshal(candidate, &objects) == nil {
			return objects, nil
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(candidate, &nested) != nil {
			continue
		}
		for _, key := range keys {
			if value := nested[key]; len(value) != 0 && json.Unmarshal(value, &objects) == nil {
				return objects, nil
			}
		}
	}
	return nil, fmt.Errorf("CGV provider response has no %s array", strings.Join(keys, ", "))
}

func cgvTheaterSourceKey(siteNo string) string {
	return strings.TrimSpace(siteNo)
}

func cgvMovieSourceKey(movNo string) string {
	return strings.TrimSpace(movNo)
}

func cgvAuditoriumSourceKey(siteNo, scnsNo string) string {
	return strings.Join([]string{strings.TrimSpace(siteNo), strings.TrimSpace(scnsNo)}, "/")
}

func cgvShowtimeSourceKey(siteNo, scnYmd, scnsNo, scnSseq string) string {
	if canonicalDate := canonicalCGVDate(scnYmd); canonicalDate != "" {
		scnYmd = canonicalDate
	}
	return strings.Join([]string{
		strings.TrimSpace(siteNo), strings.TrimSpace(scnYmd),
		strings.TrimSpace(scnsNo), strings.TrimSpace(scnSseq),
	}, "/")
}

// canonicalCGVDate accepts the ISO date used by our contracts and the compact
// YYYYMMDD form returned by CGV's booking API. Provider identity must use one
// representation so aliases cannot create two showtimes for one screening.
func canonicalCGVDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format(time.DateOnly)
		}
	}
	return ""
}

func validCGVProviderKeys(siteNo, movNo, scnsNo, scnYmd, scnSseq string) bool {
	for _, value := range []string{siteNo, movNo, scnsNo, scnYmd, scnSseq} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func parseCGVSiteResponse(payload []byte) ([]CatalogTheater, error) {
	objects, err := cgvArray(payload, "siteInfo", "sites", "siteList")
	if err != nil {
		return nil, err
	}
	result := make([]CatalogTheater, 0, len(objects))
	for _, object := range objects {
		siteNo := cgvField(object, "siteNo", "siteNO", "siteId")
		name := cgvField(object, "siteNm", "siteName", "name")
		regionCode := cgvField(object, "regnGrpCd")
		region := cgvField(object, "regnGrpNm", "regionName", "regnNm")
		if region == "" {
			region = cgvRegionNames[regionCode]
		}
		if region == "" {
			region = regionCode
		}
		if siteNo == "" || name == "" {
			return nil, fmt.Errorf("CGV site response contains an incomplete site")
		}
		result = append(result, CatalogTheater{
			SourceKey: cgvTheaterSourceKey(siteNo), Region: region, Name: name,
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("CGV site response is empty")
	}
	return result, nil
}

func parseCGVScheduleResponse(payload []byte, date string, theater ScheduleTheater) ([]scheduleEntry, error) {
	objects, err := cgvArray(payload, "schedule", "schedules", "list")
	if err != nil {
		return nil, err
	}
	entries := make([]scheduleEntry, 0, len(objects))
	for _, object := range objects {
		record := cgvScheduleRecord{
			SiteNo:       cgvField(object, "siteNo", "siteNO", "siteId"),
			ScnsNo:       cgvField(object, "scnsNo", "screenNo", "screenId"),
			ScnYmd:       cgvField(object, "scnYmd", "screenDate", "date"),
			ScnSseq:      cgvField(object, "scnSseq", "screenSequence", "sequence"),
			ProdNo:       cgvField(object, "prodNo", "productNo"),
			MovNo:        cgvField(object, "movNo", "movieNo", "movieId"),
			MovfNo:       cgvField(object, "movfNo", "movieFormatNo"),
			MovieTitle:   cgvField(object, "movNm", "movieNm", "movieTitle", "title"),
			MovieName:    cgvField(object, "movieName"),
			Auditorium:   cgvField(object, "scnsNm", "screenName", "auditoriumName"),
			AuditoriumNo: cgvField(object, "scnNm", "theaterName"),
			StartsAt:     cgvField(object, "scnsrtTm", "scnStartTime", "startTime", "startAt"),
			EndsAt:       cgvField(object, "scnendTm", "scnEndTime", "endTime", "endAt"),
			Available:    cgvInt(object, "frSeatCnt", "remainSeatCnt", "availableSeats", "seatCnt"),
			Capacity:     cgvInt(object, "stcnt", "totSeatCnt", "capacity", "totalSeats"),
			SoldOut:      cgvField(object, "soldOutYn", "scnSoldoutYn") == "Y",
		}
		if record.MovieTitle == "" {
			record.MovieTitle = record.MovieName
		}
		if record.ScnYmd == "" {
			record.ScnYmd = date
		}
		canonicalDate := canonicalCGVDate(record.ScnYmd)
		if canonicalDate == "" {
			return nil, fmt.Errorf("CGV schedule response contains an invalid scnYmd %q", record.ScnYmd)
		}
		record.ScnYmd = canonicalDate
		if !validCGVProviderKeys(record.SiteNo, record.MovNo, record.ScnsNo, record.ScnYmd, record.ScnSseq) {
			return nil, fmt.Errorf("CGV schedule response contains an incomplete provider key")
		}
		if record.SiteNo != strings.TrimSpace(theater.SourceKey) {
			return nil, fmt.Errorf("CGV schedule response site %q does not match theater %q", record.SiteNo, theater.SourceKey)
		}
		entries = append(entries, scheduleEntry{Showtime: scheduleShowtimeFromProviderRecord(record, theater)})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("CGV schedule response is empty")
	}
	return entries, nil
}

func scheduleShowtimeFromProviderRecord(record cgvScheduleRecord, theater ScheduleTheater) ScheduleShowtime {
	auditoriumKey := cgvAuditoriumSourceKey(record.SiteNo, record.ScnsNo)
	showtimeKey := cgvShowtimeSourceKey(record.SiteNo, record.ScnYmd, record.ScnsNo, record.ScnSseq)
	movieKey := cgvMovieSourceKey(record.MovNo)
	return ScheduleShowtime{
		ID: contractsCatalogID("showtime", showtimeKey), ProviderID: contractsProviderCGV,
		SourceKey: showtimeKey, TheaterID: theater.ID, MovieID: contractsCatalogID("movie", movieKey),
		MovieSourceKey: movieKey, MovieTitle: record.MovieTitle,
		AuditoriumID: contractsCatalogID("auditorium", auditoriumKey), AuditoriumSourceKey: auditoriumKey,
		AuditoriumName: firstNonEmpty(record.Auditorium, record.AuditoriumNo), ScreenTypes: detectScreenTypes(record.Auditorium),
		Date: record.ScnYmd, StartsAt: normalizeClock(record.StartsAt),
		EndsAt: normalizeClock(record.EndsAt), AvailableSeats: record.Available, Capacity: record.Capacity,
		SoldOut: record.SoldOut || (record.Capacity > 0 && record.Available == 0), SourceLabel: "",
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Small indirections keep this parser fixture-testable without importing the
// versioned contracts module from the parser's synthetic JSON helpers.
var contractsCatalogID = func(kind, source string) string { return contracts.CatalogID(contracts.ProviderCGV, kind, source) }
var contractsProviderCGV = contracts.ProviderCGV

func normalizeClock(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 4 && !strings.Contains(value, ":") {
		return value[:2] + ":" + value[2:]
	}
	return value
}
