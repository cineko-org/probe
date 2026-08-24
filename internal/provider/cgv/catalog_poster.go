package cgv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
)

const (
	maximumCatalogPosterBytes = 32 << 20
	posterCaptureSettleTime   = 12 * time.Second
	posterCapturePollInterval = 100 * time.Millisecond
)

type catalogPosterElement struct {
	Alt string `json:"alt"`
	Src string `json:"src"`
}

// captureBookingMovies observes only requests initiated while Chromium is
// rendering CGV's movie-booking page. It never fetches or navigates to an
// asset URL.
func (adapter *Adapter) captureBookingMovies(
	ctx context.Context,
	cachedPosterMovieIDs []string,
) ([]CatalogMovie, []CatalogPoster, error) {
	adapter.beginCatalogPosterCapture(cachedPosterMovieIDs)
	defer adapter.clearCatalogPosterCapture()
	if err := adapter.navigate(bookingMovieURL); err != nil {
		return nil, nil, fmt.Errorf("open CGV movie-booking catalog: %w", err)
	}
	posterImages := adapter.page.Locator(`img[alt$="포스터"]`)
	if err := posterImages.First().WaitFor(playwright.LocatorWaitForOptions{
		State: playwright.WaitForSelectorStateAttached, Timeout: playwright.Float(8_000),
	}); err != nil {
		return nil, nil, fmt.Errorf("wait for CGV movie-booking catalog: %w", err)
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return nil, nil, err
	}
	movies, err := adapter.bookingMovies()
	if err != nil {
		return nil, nil, err
	}
	// Scan tasks intentionally block stylesheets, so CGV's lazy poster images
	// can be laid out as a long document instead of a working carousel. Expose
	// each page-owned <img> to the viewport and let Chromium's image loader issue
	// its normal request. Never fetch or navigate to the CDN asset ourselves.
	if err := adapter.renderBookingPosterImages(posterImages, movies); err != nil {
		return nil, nil, err
	}
	if !isCGVMovieBookingPage(adapter.page.URL()) {
		return nil, nil, fmt.Errorf("%w: poster capture left CGV movie-booking page for %q", ErrUIContractChanged, adapter.page.URL())
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	posters := adapter.collectedCatalogPosters()
	if adapter.logger != nil {
		expected := len(movies) - adapter.cachedCatalogMovieCount(movies)
		missing := expected - len(posters)
		if missing < 0 {
			missing = 0
		}
		level := slog.LevelInfo
		outcome := "succeeded"
		if missing > 0 {
			level = slog.LevelWarn
			outcome = "unexpected"
		}
		adapter.logger.Log(ctx, level, "CGV catalog poster capture completed",
			"event", "cgv.catalog.poster_capture.completed",
			"scenario", "poster_collection",
			"operation", "capture_catalog_posters",
			"outcome", outcome,
			"expected", fmt.Sprintf("%d uncached posters captured", expected),
			"observed", fmt.Sprintf("%d uncached posters captured", len(posters)),
			"page_url", adapter.page.URL(),
			"movie_count", len(movies),
			"expected_poster_count", expected,
			"captured_poster_count", len(posters),
			"missing_poster_count", missing,
		)
	}
	return movies, posters, nil
}

func (adapter *Adapter) renderBookingPosterImages(images playwright.Locator, movies []CatalogMovie) error {
	_, err := images.EvaluateAll(`elements => {
		for (const element of elements) {
			if (element instanceof HTMLImageElement) element.loading = 'eager';
		}
		return elements.length;
	}`)
	if err != nil {
		return fmt.Errorf("render CGV movie-booking poster images: %w", err)
	}
	deadline := time.Now().Add(posterCaptureSettleTime)
	for !adapter.catalogPostersComplete(movies) && time.Now().Before(deadline) {
		if err := adapter.wait(posterCapturePollInterval); err != nil {
			return err
		}
	}
	return nil
}

func isCGVMovieBookingPage(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Hostname(), "cgv.co.kr") {
		return false
	}
	return path.Clean(parsed.Path) == "/cnm/movieBook/movie"
}

func (adapter *Adapter) bookingMovies() ([]CatalogMovie, error) {
	raw, err := adapter.page.Locator(`img[alt$="포스터"]`).EvaluateAll(`elements => elements.map(element => ({
		alt: element.getAttribute('alt') || '',
		src: element.getAttribute('src') || ''
	}))`)
	if err != nil {
		return nil, fmt.Errorf("read CGV movie-booking cards: %w", err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode CGV movie-booking cards: %w", err)
	}
	var elements []catalogPosterElement
	if err := json.Unmarshal(encoded, &elements); err != nil {
		return nil, fmt.Errorf("decode CGV movie-booking cards: %w", err)
	}
	movies := make([]CatalogMovie, 0, len(elements))
	seen := make(map[string]struct{}, len(elements))
	for _, element := range elements {
		movieSource, ok := catalogPosterMovieSource(element.Src)
		title := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(element.Alt), "포스터"))
		if !ok || title == "" {
			continue
		}
		if _, exists := seen[movieSource]; exists {
			continue
		}
		seen[movieSource] = struct{}{}
		movies = append(movies, CatalogMovie{SourceKey: movieSource, Title: title})
	}
	if len(movies) == 0 {
		return nil, fmt.Errorf("%w: CGV movie-booking page exposed no movie posters", ErrUIContractChanged)
	}
	return movies, nil
}

func (adapter *Adapter) beginCatalogPosterCapture(cachedMovieIDs []string) {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	adapter.catalogPosterMode = true
	adapter.cachedPosterMovies = make(map[string]struct{}, len(cachedMovieIDs))
	adapter.catalogPosters = make(map[string]CatalogPoster)
	for _, movieID := range cachedMovieIDs {
		if movieID = strings.TrimSpace(movieID); movieID != "" {
			adapter.cachedPosterMovies[movieID] = struct{}{}
		}
	}
}

func (adapter *Adapter) cachedCatalogMovieCount(movies []CatalogMovie) int {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	count := 0
	for _, movie := range movies {
		if _, cached := adapter.cachedPosterMovies[providerCatalogID("movie", movie.SourceKey)]; cached {
			count++
		}
	}
	return count
}

func (adapter *Adapter) clearCatalogPosterCapture() {
	adapter.catalogPosterMu.Lock()
	adapter.catalogPosterMode = false
	adapter.cachedPosterMovies = nil
	adapter.catalogPosters = nil
	adapter.catalogPosterMu.Unlock()
}

func (adapter *Adapter) catalogPosterRequestAllowed(rawURL, resourceType string) (bool, bool) {
	if !strings.EqualFold(strings.TrimSpace(resourceType), "image") {
		return false, false
	}
	_, ok := catalogPosterMovieSource(rawURL)
	if !ok {
		return false, false
	}
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	if !adapter.catalogPosterMode {
		return false, false
	}
	// Never abort a page-owned poster request merely because the local cache already
	// has the image: CGV removes cards after image errors. Let Chromium render
	// it normally; captureCatalogPosterResponse discards cached response bodies.
	return true, true
}

func (adapter *Adapter) captureCatalogPosterResponse(response playwright.Response) {
	movieSource, ok := catalogPosterResponseMovieSource(response)
	if !ok {
		return
	}
	movieID := providerCatalogID("movie", movieSource)
	if adapter.catalogPosterAlreadyAvailable(movieID) {
		return
	}
	poster, ok := adapter.readCatalogPosterResponse(response, movieSource)
	if !ok || !adapter.storeCatalogPoster(movieID, poster) {
		return
	}
	adapter.logCatalogPosterCaptured(movieID, poster)
}

func catalogPosterResponseMovieSource(response playwright.Response) (string, bool) {
	if response == nil || response.Request() == nil || !strings.EqualFold(response.Request().ResourceType(), "image") {
		return "", false
	}
	movieSource, ok := catalogPosterMovieSource(response.URL())
	return movieSource, ok && response.Status() >= 200 && response.Status() < 300
}

func (adapter *Adapter) catalogPosterAlreadyAvailable(movieID string) bool {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	_, cached := adapter.cachedPosterMovies[movieID]
	_, captured := adapter.catalogPosters[movieID]
	return !adapter.catalogPosterMode || cached || captured
}

func (adapter *Adapter) readCatalogPosterResponse(response playwright.Response, movieSource string) (CatalogPoster, bool) {
	mediaType, err := response.HeaderValue("content-type")
	if err != nil {
		adapter.logCatalogPosterDiscarded(movieSource, "content_type_unavailable", 0, err)
		return CatalogPoster{}, false
	}
	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil || !supportedPosterMediaType(mediaType) {
		adapter.logCatalogPosterDiscarded(movieSource, "unsupported_content_type", 0, err)
		return CatalogPoster{}, false
	}
	data, err := response.Body()
	if err != nil {
		adapter.logCatalogPosterDiscarded(movieSource, "response_body_unavailable", 0, err)
		return CatalogPoster{}, false
	}
	if len(data) == 0 {
		adapter.logCatalogPosterDiscarded(movieSource, "empty_response_body", 0, nil)
		return CatalogPoster{}, false
	}
	if len(data) > maximumCatalogPosterBytes {
		adapter.logCatalogPosterDiscarded(movieSource, "response_too_large", len(data), nil)
		return CatalogPoster{}, false
	}
	digest := sha256.Sum256(data)
	return CatalogPoster{
		MovieSourceKey: movieSource,
		MediaType:      strings.ToLower(mediaType),
		Data:           append([]byte(nil), data...),
		ContentHash:    hex.EncodeToString(digest[:]),
	}, true
}

func (adapter *Adapter) storeCatalogPoster(movieID string, poster CatalogPoster) bool {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	if !adapter.catalogPosterMode {
		return false
	}
	if _, cached := adapter.cachedPosterMovies[movieID]; cached {
		return false
	}
	if _, exists := adapter.catalogPosters[movieID]; exists {
		return false
	}
	adapter.catalogPosters[movieID] = poster
	return true
}

func (adapter *Adapter) logCatalogPosterCaptured(movieID string, poster CatalogPoster) {
	if adapter.logger == nil {
		return
	}
	adapter.logger.Info("CGV catalog poster captured",
		"event", "cgv.catalog.poster.captured",
		"scenario", "poster_collection",
		"operation", "capture_catalog_poster_response",
		"outcome", "succeeded",
		"movie_id", movieID,
		"movie_source_key", poster.MovieSourceKey,
		"media_type", poster.MediaType,
		"response_bytes", len(poster.Data),
		"content_hash", poster.ContentHash,
	)
}

func (adapter *Adapter) logCatalogPosterDiscarded(movieSource, reason string, responseBytes int, err error) {
	if adapter.logger == nil {
		return
	}
	attributes := []any{
		"event", "cgv.catalog.poster.discarded",
		"scenario", "poster_collection",
		"operation", "capture_catalog_poster_response",
		"outcome", "discarded",
		"expected", "supported non-empty CGV page-owned poster response",
		"observed", reason,
		"reason", reason,
		"movie_id", providerCatalogID("movie", movieSource),
		"movie_source_key", movieSource,
		"response_bytes", responseBytes,
		"maximum_response_bytes", maximumCatalogPosterBytes,
	}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	adapter.logger.Warn("CGV catalog poster discarded", attributes...)
}

func (adapter *Adapter) catalogPostersComplete(movies []CatalogMovie) bool {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	for _, movie := range movies {
		movieID := providerCatalogID("movie", movie.SourceKey)
		if _, cached := adapter.cachedPosterMovies[movieID]; cached {
			continue
		}
		if _, captured := adapter.catalogPosters[movieID]; !captured {
			return false
		}
	}
	return true
}

func (adapter *Adapter) collectedCatalogPosters() []CatalogPoster {
	adapter.catalogPosterMu.Lock()
	defer adapter.catalogPosterMu.Unlock()
	result := make([]CatalogPoster, 0, len(adapter.catalogPosters))
	for _, poster := range adapter.catalogPosters {
		poster.Data = append([]byte(nil), poster.Data...)
		result = append(result, poster)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].MovieSourceKey < result[right].MovieSourceKey
	})
	return result
}

func catalogPosterMovieSource(rawURL string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Hostname(), "cdn.cgv.co.kr") {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 7 || !strings.EqualFold(strings.Join(segments[:4], "/"), "cgvpomsfilm/Movie/Thumbnail/Poster") {
		return "", false
	}
	movieSource := segments[5]
	if !numericIdentifier(movieSource) {
		return "", false
	}
	filename := path.Base(segments[6])
	extension := strings.ToLower(path.Ext(filename))
	if !strings.HasPrefix(filename, movieSource+"_") || (extension != ".jpg" && extension != ".jpeg" && extension != ".png" && extension != ".webp") {
		return "", false
	}
	return movieSource, true
}

func supportedPosterMediaType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}
