package cgv

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

type CatalogMovie struct {
	SourceKey string
	Title     string
}

type CatalogPoster struct {
	MovieSourceKey string
	MediaType      string
	Data           []byte
	ContentHash    string
}

type CatalogTheater struct {
	SourceKey string
	Region    string
	Name      string
}

type CatalogCapture struct {
	Movies   []CatalogMovie
	Posters  []CatalogPoster
	Theaters []CatalogTheater
}

// CaptureCatalog returns provider-keyed theaters and the currently bookable
// movie catalog exposed by CGV's movie-booking page.
func (adapter *Adapter) CaptureCatalog(ctx context.Context, cachedPosterMovieIDs []string) (CatalogCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CatalogCapture{}, err
	}
	theaters, err := adapter.captureTheaters(ctx)
	if err != nil {
		return CatalogCapture{}, err
	}
	movies, posters, err := adapter.captureBookingMovies(ctx, cachedPosterMovieIDs)
	if err != nil {
		return CatalogCapture{}, err
	}
	return CatalogCapture{Theaters: theaters, Movies: movies, Posters: posters}, nil
}

func (adapter *Adapter) captureTheaters(ctx context.Context) ([]CatalogTheater, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	adapter.resetProviderResponses()
	if err := adapter.navigate(bookingCinemaURL); err != nil {
		return nil, fmt.Errorf("open CGV theater catalog: %w", err)
	}
	if err := adapter.wait(1200 * time.Millisecond); err != nil {
		return nil, err
	}
	rows, err := adapter.captureTheaterRows()
	if errors.Is(err, errTheaterResponseMissing) {
		opened, openErr := adapter.clickButtonExact("자주가는 CGV 목록 수정")
		if openErr != nil {
			return nil, openErr
		}
		if !opened {
			return nil, fmt.Errorf("%w: theater response was not captured", ErrUIContractChanged)
		}
		if err := adapter.wait(200 * time.Millisecond); err != nil {
			return nil, err
		}
		rows, err = adapter.captureTheaterRows()
	}
	if err != nil {
		return nil, err
	}
	theaters := make([]CatalogTheater, 0, len(rows))
	for _, row := range rows {
		theaters = append(theaters, CatalogTheater{
			SourceKey: row.SiteNo, Region: row.Region, Name: row.SiteName,
		})
	}
	if len(theaters) == 0 {
		return nil, fmt.Errorf("%w: theater catalog is empty", ErrUIContractChanged)
	}
	sort.Slice(theaters, func(left, right int) bool {
		if theaters[left].Region == theaters[right].Region {
			return theaters[left].Name < theaters[right].Name
		}
		return theaters[left].Region < theaters[right].Region
	})
	return theaters, nil
}
