package networkcapture

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestListReadExchangeAndBodyPath(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Save(context.Background(), Record{
		Exchange: Exchange{ID: "first", Service: "probe", Transport: "chromium", Outcome: "failed",
			CompletedAt: time.Now().Add(-time.Second), Request: Request{Method: "GET", URL: "https://cgv.test/one"},
			Response: &Response{Status: 429}},
		ResponseBody: []byte("limited"), ResponseContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), Record{
		Exchange: Exchange{ID: "second", Service: "client", Transport: "chromium", Outcome: "succeeded",
			CompletedAt: time.Now(), Request: Request{Method: "GET", URL: "https://cgv.test/two"},
			Response: &Response{Status: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := List(root, Query{Status: 429})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != first.ID || results[0].ResponseBodyBytes != 7 {
		t.Fatalf("429 results = %+v", results)
	}
	exchange, err := ReadExchange(root, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	bodyPath, body, err := BodyPath(root, exchange, "response")
	if err != nil {
		t.Fatal(err)
	}
	if body.SHA256 == "" {
		t.Fatal("body hash is empty")
	}
	if contents, err := os.ReadFile(bodyPath); err != nil || string(contents) != "limited" { // #nosec G304 -- BodyPath validates the temporary capture root.
		t.Fatalf("body = %q, %v", contents, err)
	}
	if _, _, err := BodyPath(root, exchange, "request"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing request body error = %v", err)
	}
}

func TestStatsCountsCurrentCGVRequestsSeparatelyFromBlockedAndOld(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil, WithDebug(true))
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Now()
	records := []Exchange{
		{ID: "old", Service: "probe", Transport: "chromium", Outcome: "failed", CompletedAt: boundary.Add(-time.Second), Request: Request{Method: "POST", URL: "https://cgv.co.kr/api"}, Response: &Response{Status: 429}},
		{ID: "sent", Service: "probe", Transport: "chromium", Outcome: "succeeded", CompletedAt: boundary.Add(time.Second), Request: Request{Method: "POST", URL: "https://www.cgv.co.kr/api"}, Response: &Response{Status: 200}},
		{ID: "blocked", Service: "probe", Transport: "chromium", Outcome: "blocked", CompletedAt: boundary.Add(2 * time.Second), Request: Request{Method: "GET", URL: "https://cdn.cgv.co.kr/poster"}},
		{ID: "limited", Service: "probe", Transport: "chromium", Outcome: "failed", CompletedAt: boundary.Add(3 * time.Second), Request: Request{Method: "POST", URL: "https://cgv.co.kr/api"}, Response: &Response{Status: 429}},
		{ID: "local", Service: "client", Transport: "http_server", Outcome: "succeeded", CompletedAt: boundary.Add(4 * time.Second), Request: Request{Method: "GET", URL: "http://127.0.0.1/api"}, Response: &Response{Status: 200}},
	}
	for _, exchange := range records {
		if _, err := store.Save(context.Background(), Record{Exchange: exchange}); err != nil {
			t.Fatal(err)
		}
	}
	statistics, err := Stats(root, Query{CompletedAfter: boundary})
	if err != nil {
		t.Fatal(err)
	}
	if statistics.Captured != 4 || statistics.ProviderSent != 2 || statistics.Blocked != 1 || statistics.Failed != 1 || statistics.Status429 != 1 {
		t.Fatalf("statistics = %+v", statistics)
	}
}
