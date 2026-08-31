package networkcapture

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultListLimit = 100
	maximumListLimit = 500
	maximumIndexLine = 4 << 20
	maximumIndexTail = 64 << 20
)

type Summary struct {
	Version            int       `json:"version"`
	ID                 string    `json:"id"`
	CompletedAt        time.Time `json:"completed_at"`
	Service            string    `json:"service"`
	Scenario           string    `json:"scenario,omitempty"`
	Transport          string    `json:"transport"`
	Outcome            string    `json:"outcome"`
	Method             string    `json:"method"`
	URL                string    `json:"url"`
	Status             int       `json:"status"`
	DurationMS         int64     `json:"duration_ms"`
	ManifestPath       string    `json:"manifest_path"`
	Error              string    `json:"error,omitempty"`
	CaptureError       string    `json:"capture_error,omitempty"`
	RequestBodySHA256  string    `json:"request_body_sha256,omitempty"`
	RequestBodyBytes   int64     `json:"request_body_bytes,omitempty"`
	ResponseBodySHA256 string    `json:"response_body_sha256,omitempty"`
	ResponseBodyBytes  int64     `json:"response_body_bytes,omitempty"`
}

type Query struct {
	Limit          int
	MinimumStatus  int
	Status         int
	Outcome        string
	URLContains    string
	CompletedAfter time.Time
}

type Statistics struct {
	Captured     int  `json:"captured"`
	ProviderSent int  `json:"provider_sent"`
	Blocked      int  `json:"blocked"`
	Failed       int  `json:"failed"`
	Status429    int  `json:"status_429"`
	Truncated    bool `json:"truncated"`
}

// Stats counts every matching exchange in the bounded durable index rather
// than only the visible List page.
func Stats(root string, query Query) (Statistics, error) {
	indexPath := filepath.Join(filepath.Clean(root), "index.jsonl")
	file, err := os.Open(indexPath) // #nosec G304 -- root is the application-owned data directory.
	if errors.Is(err, os.ErrNotExist) {
		return Statistics{}, nil
	}
	if err != nil {
		return Statistics{}, fmt.Errorf("open network capture index: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Statistics{}, fmt.Errorf("stat network capture index: %w", err)
	}
	offset := int64(0)
	if info.Size() > maximumIndexTail {
		offset = info.Size() - maximumIndexTail
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return Statistics{}, fmt.Errorf("seek network capture index: %w", err)
		}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maximumIndexLine)
	if offset > 0 {
		_ = scanner.Scan()
	}
	statistics := Statistics{Truncated: offset > 0}
	for scanner.Scan() {
		var summary Summary
		if json.Unmarshal(scanner.Bytes(), &summary) != nil || !matchesSummary(summary, query) {
			continue
		}
		addSummaryStats(&statistics, summary)
	}
	if err := scanner.Err(); err != nil {
		return Statistics{}, fmt.Errorf("scan network capture index: %w", err)
	}
	return statistics, nil
}

func addSummaryStats(statistics *Statistics, summary Summary) {
	statistics.Captured++
	if strings.EqualFold(summary.Outcome, "blocked") || strings.EqualFold(summary.Outcome, "canceled") {
		statistics.Blocked++
	} else if isCGVURL(summary.URL) {
		statistics.ProviderSent++
	}
	if strings.EqualFold(summary.Outcome, "failed") || summary.Status >= 400 {
		statistics.Failed++
	}
	if summary.Status == 429 {
		statistics.Status429++
	}
}

func isCGVURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "cgv.co.kr" || host == "www.cgv.co.kr"
}

func List(root string, query Query) ([]Summary, error) {
	indexPath := filepath.Join(filepath.Clean(root), "index.jsonl")
	file, err := os.Open(indexPath) // #nosec G304 -- root is the application-owned data directory.
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open network capture index: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat network capture index: %w", err)
	}
	offset := int64(0)
	if info.Size() > maximumIndexTail {
		offset = info.Size() - maximumIndexTail
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek network capture index: %w", err)
		}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maximumIndexLine)
	if offset > 0 {
		_ = scanner.Scan()
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maximumListLimit {
		limit = maximumListLimit
	}
	result := make([]Summary, 0, limit)
	for scanner.Scan() {
		var summary Summary
		if json.Unmarshal(scanner.Bytes(), &summary) != nil || !matchesSummary(summary, query) {
			continue
		}
		if len(result) == limit {
			copy(result, result[1:])
			result[len(result)-1] = summary
			continue
		}
		result = append(result, summary)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan network capture index: %w", err)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].CompletedAt.After(result[right].CompletedAt) })
	return result, nil
}

func ReadExchange(root, id string) (Exchange, error) {
	id = strings.TrimSpace(id)
	if id == "" || safeID(id) != id {
		return Exchange{}, errors.New("invalid network exchange id")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Clean(root), "exchanges", "*", id+".json"))
	if err != nil {
		return Exchange{}, fmt.Errorf("find network exchange: %w", err)
	}
	if len(matches) == 0 {
		return Exchange{}, os.ErrNotExist
	}
	if len(matches) > 1 {
		return Exchange{}, errors.New("network exchange id is not unique")
	}
	contents, err := os.ReadFile(matches[0]) // #nosec G304 -- matched below the application-owned capture root.
	if err != nil {
		return Exchange{}, fmt.Errorf("read network exchange: %w", err)
	}
	var exchange Exchange
	if err := json.Unmarshal(contents, &exchange); err != nil {
		return Exchange{}, fmt.Errorf("decode network exchange: %w", err)
	}
	return exchange, nil
}

func BodyPath(root string, exchange Exchange, side string) (string, *Body, error) {
	var body *Body
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "request":
		body = exchange.Request.Body
	case "response":
		if exchange.Response != nil {
			body = exchange.Response.Body
		}
	default:
		return "", nil, errors.New("body side must be request or response")
	}
	if body == nil || strings.TrimSpace(body.Path) == "" {
		return "", nil, os.ErrNotExist
	}
	cleanRoot := filepath.Clean(root)
	path := filepath.Join(cleanRoot, filepath.Clean(body.Path))
	relative, err := filepath.Rel(cleanRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("network body path escapes capture root")
	}
	return path, body, nil
}

func matchesSummary(summary Summary, query Query) bool {
	if !query.CompletedAfter.IsZero() && summary.CompletedAt.Before(query.CompletedAfter) {
		return false
	}
	if query.Status > 0 && summary.Status != query.Status {
		return false
	}
	if query.MinimumStatus > 0 && summary.Status < query.MinimumStatus {
		return false
	}
	if outcome := strings.TrimSpace(query.Outcome); outcome != "" && !strings.EqualFold(summary.Outcome, outcome) {
		return false
	}
	if value := strings.ToLower(strings.TrimSpace(query.URLContains)); value != "" && !strings.Contains(strings.ToLower(summary.URL), value) {
		return false
	}
	return true
}
