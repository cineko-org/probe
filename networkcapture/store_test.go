package networkcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePersistsCompleteExchangeAndDeduplicatesBodies(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("rate limited\n", 2048))
	record := Record{
		Exchange: Exchange{
			CorrelationID: "operation-1", Service: "probe", Scenario: "fixture",
			Transport: "chromium", StartedAt: time.Now().Add(-time.Second),
			Outcome: "failed", Error: "HTTP 429", CaptureError: "response body unavailable",
			Request:  Request{Method: "POST", URL: "https://example.test/api?date=20260829", Headers: []Header{{Name: "Cookie", Value: "session=value"}}},
			Response: &Response{Status: 429, StatusText: "Too Many Requests", Protocol: "h2", Headers: []Header{{Name: "Retry-After", Value: "60"}}},
		},
		RequestBody: []byte(`{"theater":"0013"}`), RequestContentType: "application/json",
		ResponseBody: body, ResponseContentType: "text/plain", ResponseRepresentation: "decoded",
	}
	first, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResponseBody.SHA256 != second.ResponseBody.SHA256 || first.ResponseBody.Path != second.ResponseBody.Path {
		t.Fatalf("body was not content-addressed: first=%+v second=%+v", first.ResponseBody, second.ResponseBody)
	}
	blobs, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(blobs), 2; got != want {
		t.Fatalf("blob count = %d, want %d (one request and one shared response body)", got, want)
	}
	manifestBytes, err := os.ReadFile(first.ManifestPath) // #nosec G304 -- manifest path is returned by the temporary test store.
	if err != nil {
		t.Fatal(err)
	}
	var manifest Exchange
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Request.URL != record.Request.URL || manifest.Response == nil || manifest.Response.Status != 429 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if manifest.CaptureError != record.CaptureError {
		t.Fatalf("capture error = %q, want %q", manifest.CaptureError, record.CaptureError)
	}
	indexBytes, err := os.ReadFile(filepath.Join(root, "index.jsonl")) // #nosec G304 -- root is a testing.T temp directory.
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(indexBytes)), "\n")); got != 2 {
		t.Fatalf("index entries = %d, want 2", got)
	}
	if !strings.Contains(string(indexBytes), `"capture_error":"response body unavailable"`) {
		t.Fatalf("index omitted capture error: %s", indexBytes)
	}
}

func TestStoreStressTenThousandCapturesWithoutDrops(t *testing.T) {
	if os.Getenv("CINEKO_STRESS_NETWORK_CAPTURE") != "1" {
		t.Skip("set CINEKO_STRESS_NETWORK_CAPTURE=1 to run the 10k durability fixture")
	}
	root := t.TempDir()
	store, err := NewStore(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("x", 33<<10))
	const workers, perWorker = 20, 500
	var wait sync.WaitGroup
	errorsFound := make(chan error, workers)
	for worker := range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range perWorker {
				id := fmt.Sprintf("stress-%02d-%04d", worker, index)
				_, err := store.Save(context.Background(), Record{
					Exchange:     Exchange{ID: id, Service: "fixture", Transport: "http", Request: Request{Method: "GET", URL: "http://fixture.test"}, Response: &Response{Status: 429}},
					ResponseBody: body, ResponseContentType: "application/octet-stream",
				})
				if err != nil {
					errorsFound <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	manifests, err := filepath.Glob(filepath.Join(root, "exchanges", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != workers*perWorker {
		t.Fatalf("manifest count = %d, want %d", len(manifests), workers*perWorker)
	}
	blobs, err := filepath.Glob(filepath.Join(root, "blobs", "sha256", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("deduplicated blob count = %d, want 1", len(blobs))
	}
}

func BenchmarkStoreRepeated33KiBBody(b *testing.B) {
	store, err := NewStore(b.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), WithDebug(true))
	if err != nil {
		b.Fatal(err)
	}
	body := []byte(strings.Repeat("x", 33<<10))
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for index := range b.N {
		_, err := store.Save(context.Background(), Record{
			Exchange:     Exchange{ID: fmt.Sprintf("bench-%08d", index), Service: "fixture", Transport: "http", Request: Request{Method: "GET", URL: "http://fixture.test"}, Response: &Response{Status: 200}},
			ResponseBody: body,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestStoreRejectsCanceledCaptureWithoutWriting(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Save(ctx, Record{}); err == nil {
		t.Fatal("Save succeeded with canceled context")
	}
	if _, err := os.Stat(filepath.Join(root, "index.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("index created for canceled capture: %v", err)
	}
}

func TestStoreAcceptsNilContext(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(nil, Record{ //nolint:staticcheck // This test intentionally verifies the documented nil-context fallback.
		Exchange: Exchange{Service: "fixture", Transport: "http", Request: Request{Method: "GET", URL: "http://fixture.test"}},
	}); err != nil {
		t.Fatalf("Save(nil) failed: %v", err)
	}
}

func TestStoreDefaultModeSkipsSuccessfulExchange(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Save(context.Background(), Record{Exchange: Exchange{
		Service: "fixture", Transport: "http", Outcome: "succeeded",
		Request: Request{Method: "GET", URL: "https://cgv.test/catalog"}, Response: &Response{Status: 200},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "" {
		t.Fatalf("successful exchange persisted in default mode: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "index.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default mode created a network index: %v", err)
	}
}

func TestStoreDefaultModeSkipsExpectedBrowserCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Save(context.Background(), Record{Exchange: Exchange{
		Service: "client", Transport: "chromium", Outcome: "canceled",
		CaptureError: "response body unavailable after navigation",
		Request:      Request{Method: "GET", URL: "https://analytics.test/collect"},
		Response:     &Response{Status: 204},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "" {
		t.Fatalf("expected browser cancellation persisted in default mode: %+v", result)
	}
}

func TestStoreClearRemovesDurableCapturesAndRemainsWritable(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil, WithDebug(true))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Save(context.Background(), Record{
		Exchange: Exchange{ID: "before-clear", Service: "probe", Transport: "chromium",
			Request: Request{Method: "GET", URL: "https://cgv.co.kr/api"}, Response: &Response{Status: 200}},
		ResponseBody: []byte("body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest still exists after clear: %v", err)
	}
	entries, err := List(root, Query{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries after clear = %+v, %v", entries, err)
	}
	if _, err := store.Save(context.Background(), Record{
		Exchange: Exchange{ID: "after-clear", Service: "probe", Transport: "chromium",
			Request: Request{Method: "GET", URL: "https://cgv.co.kr/api"}, Response: &Response{Status: 200}},
	}); err != nil {
		t.Fatalf("save after clear: %v", err)
	}
}

func TestStoreClearRejectsCaptureStartedBeforeClear(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().Add(-time.Second)
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	result, err := store.Save(context.Background(), Record{Exchange: Exchange{
		ID: "late-completion", Service: "probe", Transport: "chromium", StartedAt: startedAt,
		CompletedAt: time.Now(), Request: Request{Method: "GET", URL: "https://cgv.co.kr/api"}, Response: &Response{Status: 200},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "" {
		t.Fatalf("pre-clear capture was persisted: %+v", result)
	}
	entries, err := List(root, Query{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries after late completion = %+v, %v", entries, err)
	}
}

func TestStoreStreamsStagingFileIntoDeduplicatedBlob(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil, WithDebug(true))
	if err != nil {
		t.Fatal(err)
	}
	staging, cleanup, err := store.NewStagingFile()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	contents := strings.Repeat("streamed-body", 10000)
	if _, err := staging.WriteString(contents); err != nil {
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := store.Save(context.Background(), Record{
		Exchange:         Exchange{ID: "streamed", Service: "client", Transport: "http", Request: Request{Method: "GET", URL: "http://local.test"}},
		ResponseBodyPath: staging.Name(), ResponseContentType: "text/plain", ResponseRepresentation: "application",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResponseBody == nil || result.ResponseBody.Bytes != int64(len(contents)) {
		t.Fatalf("streamed body = %+v", result.ResponseBody)
	}
	persisted, err := os.ReadFile(filepath.Join(store.Root(), result.ResponseBody.Path))
	if err != nil || string(persisted) != contents {
		t.Fatalf("persisted streamed body length/error = %d/%v", len(persisted), err)
	}
}

func TestNewStoreRemovesOnlyStaleIncompleteStagingFiles(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, ".staging")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(directory, ".body-stale")
	recent := filepath.Join(directory, ".body-recent")
	durableLooking := filepath.Join(directory, "keep-me")
	for _, path := range []string{stale, recent, durableLooking} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-stagingMaxAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale staging file still exists: %v", err)
	}
	for _, path := range []string{recent, durableLooking} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file %q: %v", path, err)
		}
	}
}
