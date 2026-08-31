// Package networkcapture persists complete local HTTP exchanges for diagnosis.
// Browser-specific adapters live in subpackages so HTTP-only consumers do not
// inherit Playwright or browser-runtime dependencies.
//
// The manifest is intentionally separate from body blobs: repeated payloads are
// content-addressed once, while every request still has its own chronological
// exchange record and JSONL index entry.
package networkcapture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	manifestVersion = 1
	stagingMaxAge   = 24 * time.Hour
)

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Body struct {
	SHA256         string `json:"sha256"`
	Bytes          int64  `json:"bytes"`
	Path           string `json:"path"`
	ContentType    string `json:"content_type,omitempty"`
	Representation string `json:"representation,omitempty"`
}

type Timing struct {
	StartMillis           float64 `json:"start_ms,omitempty"`
	DomainLookupStart     float64 `json:"domain_lookup_start_ms,omitempty"`
	DomainLookupEnd       float64 `json:"domain_lookup_end_ms,omitempty"`
	ConnectStart          float64 `json:"connect_start_ms,omitempty"`
	SecureConnectionStart float64 `json:"secure_connection_start_ms,omitempty"`
	ConnectEnd            float64 `json:"connect_end_ms,omitempty"`
	RequestStart          float64 `json:"request_start_ms,omitempty"`
	ResponseStart         float64 `json:"response_start_ms,omitempty"`
	ResponseEnd           float64 `json:"response_end_ms,omitempty"`
}

type Request struct {
	Method         string   `json:"method"`
	URL            string   `json:"url"`
	Headers        []Header `json:"headers,omitempty"`
	Body           *Body    `json:"body,omitempty"`
	ResourceType   string   `json:"resource_type,omitempty"`
	Navigation     bool     `json:"navigation,omitempty"`
	RedirectedFrom string   `json:"redirected_from,omitempty"`
	RedirectedTo   string   `json:"redirected_to,omitempty"`
	Bytes          int64    `json:"bytes,omitempty"`
	Timing         *Timing  `json:"timing,omitempty"`
}

type Response struct {
	Status            int      `json:"status"`
	StatusText        string   `json:"status_text,omitempty"`
	Protocol          string   `json:"protocol,omitempty"`
	Headers           []Header `json:"headers,omitempty"`
	Body              *Body    `json:"body,omitempty"`
	Bytes             int64    `json:"bytes,omitempty"`
	FromServiceWorker bool     `json:"from_service_worker,omitempty"`
	ServerAddress     string   `json:"server_address,omitempty"`
	ServerPort        int      `json:"server_port,omitempty"`
}

type Exchange struct {
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Service       string    `json:"service"`
	Scenario      string    `json:"scenario,omitempty"`
	Transport     string    `json:"transport"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	DurationMS    int64     `json:"duration_ms"`
	Outcome       string    `json:"outcome"`
	Error         string    `json:"error,omitempty"`
	CaptureError  string    `json:"capture_error,omitempty"`
	Request       Request   `json:"request"`
	Response      *Response `json:"response,omitempty"`
}

// Record is an exchange whose bodies have not yet been persisted.
type Record struct {
	Exchange
	RequestBody            []byte
	RequestBodyPath        string
	RequestContentType     string
	RequestRepresentation  string
	ResponseBody           []byte
	ResponseBodyPath       string
	ResponseContentType    string
	ResponseRepresentation string
}

type Result struct {
	ID           string
	ManifestPath string
	RequestBody  *Body
	ResponseBody *Body
}

type Store struct {
	root      string
	logger    *slog.Logger
	debug     bool
	mu        sync.Mutex
	clearedAt time.Time
}

type Option func(*Store)

// WithDebug enables complete successful request and response capture. Without
// it the store keeps only failed exchanges and 4xx/5xx responses, which are
// the records needed for normal operations diagnostics.
func WithDebug(enabled bool) Option {
	return func(store *Store) { store.debug = enabled }
}

func NewStore(root string, logger *slog.Logger, options ...Option) (*Store, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return nil, errors.New("network capture root is required")
	}
	for _, directory := range []string{
		filepath.Join(root, "blobs", "sha256"),
		filepath.Join(root, "exchanges"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create network capture directory %q: %w", directory, err)
		}
	}
	cleanupStaleStaging(root, time.Now())
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	store := &Store{root: root, logger: logger}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// DebugEnabled reports whether successful exchanges should retain complete
// request and response evidence.
func (store *Store) DebugEnabled() bool {
	return store != nil && store.debug
}

// cleanupStaleStaging removes only incomplete stream files left by an older
// crashed process. Durable manifests and content-addressed bodies are retained
// until the user removes ~/cineko.
func cleanupStaleStaging(root string, now time.Time) {
	directory := filepath.Join(root, ".staging")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".body-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < stagingMaxAge {
			continue
		}
		_ = os.Remove(filepath.Join(directory, entry.Name()))
	}
}

func (store *Store) Root() string {
	if store == nil {
		return ""
	}
	return store.root
}

// Clear removes durable request/response manifests, their index, and body
// blobs. Active staging streams are retained so concurrent captures can finish
// safely after the clear boundary.
func (store *Store) Clear() error {
	if store == nil {
		return errors.New("network capture store is nil")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.clearedAt = time.Now()
	for _, path := range []string{
		filepath.Join(store.root, "index.jsonl"),
		filepath.Join(store.root, "exchanges"),
		filepath.Join(store.root, "blobs"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("clear network capture path %q: %w", path, err)
		}
	}
	for _, directory := range []string{
		filepath.Join(store.root, "blobs", "sha256"),
		filepath.Join(store.root, "exchanges"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("recreate network capture directory %q: %w", directory, err)
		}
	}
	return nil
}

// Save is synchronous and returns only after the bodies, manifest, and index
// are closed and atomically visible to the App. This deliberately applies
// backpressure instead of silently dropping diagnostic evidence.
//
//nolint:gocyclo,cyclop // One lock must cover normalization, body deduplication, manifest write, and index append.
func (store *Store) Save(ctx context.Context, record Record) (Result, error) {
	if store == nil {
		return Result{}, errors.New("network capture store is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return Result{}, context.Cause(ctx)
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	now := time.Now()
	if !store.clearedAt.IsZero() {
		startedAt := record.StartedAt
		if startedAt.IsZero() {
			startedAt = record.CompletedAt
		}
		if !startedAt.IsZero() && startedAt.Before(store.clearedAt) {
			return Result{}, nil
		}
	}
	if record.ID == "" {
		record.ID = newID(now)
	}
	if record.StartedAt.IsZero() {
		record.StartedAt = now
	}
	if record.CompletedAt.IsZero() {
		record.CompletedAt = now
	}
	if record.DurationMS == 0 && record.CompletedAt.After(record.StartedAt) {
		record.DurationMS = record.CompletedAt.Sub(record.StartedAt).Milliseconds()
	}
	if record.Outcome == "" {
		record.Outcome = "succeeded"
	}
	record.Version = manifestVersion
	if !store.shouldPersist(record.Exchange) {
		return Result{}, nil
	}

	var result Result
	result.ID = record.ID
	var err error
	if record.RequestBody != nil {
		result.RequestBody, err = store.saveBody(record.RequestBody, record.RequestContentType, record.RequestRepresentation)
	} else if strings.TrimSpace(record.RequestBodyPath) != "" {
		result.RequestBody, err = store.saveBodyFile(record.RequestBodyPath, record.RequestContentType, record.RequestRepresentation)
	}
	if result.RequestBody != nil || err != nil {
		if err != nil {
			return Result{}, fmt.Errorf("persist request body: %w", err)
		}
		record.Request.Body = result.RequestBody
	}
	if record.ResponseBody != nil {
		result.ResponseBody, err = store.saveBody(record.ResponseBody, record.ResponseContentType, record.ResponseRepresentation)
	} else if strings.TrimSpace(record.ResponseBodyPath) != "" {
		result.ResponseBody, err = store.saveBodyFile(record.ResponseBodyPath, record.ResponseContentType, record.ResponseRepresentation)
	}
	if result.ResponseBody != nil || err != nil {
		if err != nil {
			return Result{}, fmt.Errorf("persist response body: %w", err)
		}
		if record.Response == nil {
			record.Response = &Response{}
		}
		record.Response.Body = result.ResponseBody
	}

	day := record.CompletedAt.In(time.Local).Format("2006-01-02")
	manifestDirectory := filepath.Join(store.root, "exchanges", day)
	if err := os.MkdirAll(manifestDirectory, 0o700); err != nil {
		return Result{}, fmt.Errorf("create exchange directory: %w", err)
	}
	manifestPath := filepath.Join(manifestDirectory, safeID(record.ID)+".json")
	encoded, err := json.MarshalIndent(record.Exchange, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode exchange manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := atomicWrite(manifestPath, encoded, 0o600); err != nil {
		return Result{}, fmt.Errorf("write exchange manifest: %w", err)
	}
	result.ManifestPath = manifestPath

	summary := map[string]any{
		"version": manifestVersion, "id": record.ID,
		"completed_at": record.CompletedAt, "service": record.Service,
		"scenario": record.Scenario, "transport": record.Transport,
		"outcome": record.Outcome, "method": record.Request.Method,
		"url": record.Request.URL, "status": responseStatus(record.Response),
		"duration_ms": record.DurationMS, "manifest_path": manifestPath,
	}
	if record.Error != "" {
		summary["error"] = record.Error
	}
	if record.CaptureError != "" {
		summary["capture_error"] = record.CaptureError
	}
	if result.RequestBody != nil {
		summary["request_body_sha256"] = result.RequestBody.SHA256
		summary["request_body_bytes"] = result.RequestBody.Bytes
	}
	if result.ResponseBody != nil {
		summary["response_body_sha256"] = result.ResponseBody.SHA256
		summary["response_body_bytes"] = result.ResponseBody.Bytes
	}
	summaryBytes, err := json.Marshal(summary)
	if err != nil {
		return Result{}, fmt.Errorf("encode exchange summary: %w", err)
	}
	index, err := os.OpenFile(filepath.Join(store.root, "index.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("open exchange index: %w", err)
	}
	_, writeErr := index.Write(append(summaryBytes, '\n'))
	closeErr := index.Close()
	if writeErr != nil {
		return Result{}, fmt.Errorf("append exchange index: %w", writeErr)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close exchange index: %w", closeErr)
	}
	store.logger.DebugContext(ctx, "Network exchange captured",
		"event", "network.exchange.captured", "network_exchange_id", record.ID,
		"request_id", record.CorrelationID, "transport", record.Transport,
		"method", record.Request.Method, "request_url", record.Request.URL,
		"status", responseStatus(record.Response), "outcome", record.Outcome,
		"duration_ms", record.DurationMS, "artifact_path", manifestPath)
	return result, nil
}

func (store *Store) shouldPersist(exchange Exchange) bool {
	if store.DebugEnabled() {
		return true
	}
	if exchange.Outcome == "blocked" || exchange.Outcome == "canceled" {
		return exchange.Response != nil && exchange.Response.Status >= 400
	}
	if exchange.Error != "" || exchange.CaptureError != "" || exchange.Outcome == "failed" {
		return true
	}
	return exchange.Response != nil && exchange.Response.Status >= 400
}

func (store *Store) saveBody(contents []byte, contentType, representation string) (*Body, error) {
	digest := sha256.Sum256(contents)
	hash := hex.EncodeToString(digest[:])
	relativePath := filepath.Join("blobs", "sha256", hash[:2], hash)
	absolutePath := filepath.Join(store.root, relativePath)
	if _, err := os.Stat(absolutePath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
			return nil, err
		}
		if err := atomicWrite(absolutePath, contents, 0o600); err != nil {
			return nil, err
		}
	}
	return &Body{
		SHA256: hash, Bytes: int64(len(contents)), Path: relativePath,
		ContentType: contentType, Representation: representation,
	}, nil
}

func (store *Store) saveBodyFile(sourcePath, contentType, representation string) (*Body, error) {
	source, err := os.Open(filepath.Clean(sourcePath)) // #nosec G304 -- caller supplies an application-owned staging file.
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	digest := sha256.New()
	bytesWritten, err := io.Copy(digest, source)
	if err != nil {
		return nil, err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	relativePath := filepath.Join("blobs", "sha256", hash[:2], hash)
	absolutePath := filepath.Join(store.root, relativePath)
	if _, err := os.Stat(absolutePath); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
			return nil, err
		}
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		temporary, err := os.CreateTemp(filepath.Dir(absolutePath), ".cineko-network-*")
		if err != nil {
			return nil, err
		}
		temporaryPath := temporary.Name()
		defer func() { _ = os.Remove(temporaryPath) }()
		if err := temporary.Chmod(0o600); err != nil {
			_ = temporary.Close()
			return nil, err
		}
		if _, err := io.Copy(temporary, source); err != nil {
			_ = temporary.Close()
			return nil, err
		}
		if err := temporary.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(temporaryPath, absolutePath); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &Body{
		SHA256: hash, Bytes: bytesWritten, Path: relativePath,
		ContentType: contentType, Representation: representation,
	}, nil
}

// NewStagingFile creates a private temporary file below the capture root.
// The caller closes it, passes its path to Record, then invokes cleanup.
func (store *Store) NewStagingFile() (*os.File, func(), error) {
	if store == nil {
		return nil, nil, errors.New("network capture store is nil")
	}
	directory := filepath.Join(store.root, ".staging")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(directory, ".body-*")
	if err != nil {
		return nil, nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	return file, cleanup, nil
}

func atomicWrite(path string, contents []byte, mode fs.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cineko-network-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func newID(now time.Time) string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("%d", now.UnixNano())
	}
	return fmt.Sprintf("%d-%s", now.UnixNano(), hex.EncodeToString(random))
}

func safeID(value string) string {
	return strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z':
			return character
		case character >= 'A' && character <= 'Z':
			return character
		case character >= '0' && character <= '9':
			return character
		case character == '-', character == '_':
			return character
		default:
			return '_'
		}
	}, value)
}

func responseStatus(response *Response) int {
	if response == nil {
		return 0
	}
	return response.Status
}
