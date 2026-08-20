package cgv

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type CatalogMovie struct {
	SourceKey string
	Title     string
	PosterURL string
}

type CatalogTheater struct {
	SourceKey string
	Region    string
	Name      string
}

type CatalogCapture struct {
	Movies   []CatalogMovie
	Theaters []CatalogTheater
}

func (adapter *Adapter) CaptureCatalog(ctx context.Context) (CatalogCapture, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CatalogCapture{}, err
	}
	theaters, err := adapter.captureTheaters(ctx)
	if err != nil {
		return CatalogCapture{}, err
	}
	// The current provider exposes stable movie identity (movNo) in schedule
	// responses. No separate movie-catalog endpoint has been verified, so the
	// full catalog task seeds theaters only; schedule captures add movies with
	// their provider keys in the same Central catalog.
	return CatalogCapture{Theaters: theaters}, nil
}

func (adapter *Adapter) captureTheaters(ctx context.Context) ([]CatalogTheater, error) {
	adapter.clearProviderResponse("sites")
	err := adapter.openTheaterCatalog()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := adapter.waitForProviderResponse("sites", 2*time.Second); err != nil {
		return nil, err
	}
	payload := adapter.providerResponse("sites")
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: CGV site API response was not captured", ErrUIContractChanged)
	}
	theaters, err := parseCGVSiteResponse(payload)
	if err != nil {
		return nil, fmt.Errorf("parse CGV site catalog: %w", err)
	}
	if len(theaters) == 0 {
		return nil, fmt.Errorf("%w: theater catalog is empty", ErrUIContractChanged)
	}
	sort.Slice(theaters, func(left, right int) bool { return theaters[left].Name < theaters[right].Name })
	return theaters, nil
}

func (adapter *Adapter) openTheaterCatalog() error {
	if err := adapter.navigate(bookingCinemaURL); err != nil {
		return fmt.Errorf("open CGV theater catalog: %w", err)
	}
	if err := adapter.wait(1200 * time.Millisecond); err != nil {
		return err
	}
	opened, err := adapter.clickButtonExact("자주가는 CGV 목록 수정")
	if err != nil {
		return err
	}
	if !opened {
		return fmt.Errorf("%w: theater picker button not found", ErrUIContractChanged)
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return err
	}
	return nil
}
