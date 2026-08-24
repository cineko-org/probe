package cgv

import "testing"

func TestCatalogPosterMovieSourceAcceptsOnlyCGVMoviePosterAssets(t *testing.T) {
	t.Parallel()
	valid := "https://cdn.cgv.co.kr/cgvpomsfilm/Movie/Thumbnail/Poster/030001/30001348/30001348_320.jpg"
	if movie, ok := catalogPosterMovieSource(valid); !ok || movie != "30001348" {
		t.Fatalf("catalogPosterMovieSource(valid) = %q, %t", movie, ok)
	}
	for _, candidate := range []string{
		"https://cgv.co.kr/cgvpomsfilm/Movie/Thumbnail/Poster/030001/30001348/30001348_320.jpg",
		"https://cdn.cgv.co.kr/cgvpomsfilm/Movie/Thumbnail/Poster/030001/30001348/other_320.jpg",
		"https://cdn.cgv.co.kr/cgvpomsfilm/Movie/Thumbnail/Poster/030001/not-a-number/not-a-number_320.jpg",
		"https://cdn.cgv.co.kr/cgvpomscontent/static/logo.png",
		"https://cdn.example/cgvpomsfilm/Movie/Thumbnail/Poster/030001/30001348/30001348_320.jpg",
	} {
		if movie, ok := catalogPosterMovieSource(candidate); ok {
			t.Fatalf("catalogPosterMovieSource(%q) unexpectedly accepted %q", candidate, movie)
		}
	}
}

func TestCatalogPosterRoutingAllowsOnlyMissingBrowserInitiatedImages(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{}
	url := "https://cdn.cgv.co.kr/cgvpomsfilm/Movie/Thumbnail/Poster/030001/30001348/30001348_320.jpg"
	if allowed, recognized := adapter.catalogPosterRequestAllowed(url, "image"); allowed || recognized {
		t.Fatalf("inactive capture decision = %t, %t", allowed, recognized)
	}
	movieID := providerCatalogID("movie", "30001348")
	adapter.beginCatalogPosterCapture([]string{movieID})
	if allowed, recognized := adapter.catalogPosterRequestAllowed(url, "image"); !allowed || !recognized {
		t.Fatalf("cached capture decision = %t, %t", allowed, recognized)
	}
	adapter.beginCatalogPosterCapture(nil)
	if allowed, recognized := adapter.catalogPosterRequestAllowed(url, "image"); !allowed || !recognized {
		t.Fatalf("missing capture decision = %t, %t", allowed, recognized)
	}
	if allowed, recognized := adapter.catalogPosterRequestAllowed(url, "fetch"); allowed || recognized {
		t.Fatalf("script fetch decision = %t, %t", allowed, recognized)
	}
}

func TestIsCGVMovieBookingPageRejectsOtherNavigation(t *testing.T) {
	t.Parallel()
	for _, candidate := range []string{
		"https://cgv.co.kr/cnm/movieBook/movie",
		"https://cgv.co.kr/cnm/movieBook/movie?tab=all",
		"https://cgv.co.kr/cnm/movieBook/movie/",
	} {
		if !isCGVMovieBookingPage(candidate) {
			t.Fatalf("isCGVMovieBookingPage(%q) = false", candidate)
		}
	}
	for _, candidate := range []string{
		"https://cgv.co.kr/",
		"https://cgv.co.kr/cnm/movieBook/cinema",
		"https://cgv.co.kr/cnm/cgvChart/movieChart",
		"https://cdn.cgv.co.kr/",
		"https://example.com/",
	} {
		if isCGVMovieBookingPage(candidate) {
			t.Fatalf("isCGVMovieBookingPage(%q) = true", candidate)
		}
	}
}
