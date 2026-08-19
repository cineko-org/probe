package cgv

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const bookingMovieURL = "https://cgv.co.kr/cnm/movieBook/movie"

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
	movies, err := adapter.captureMovies()
	if err != nil {
		return CatalogCapture{}, err
	}
	theaters, err := adapter.captureTheaters(ctx)
	if err != nil {
		return CatalogCapture{}, err
	}
	return CatalogCapture{Movies: movies, Theaters: theaters}, nil
}

func (adapter *Adapter) captureMovies() ([]CatalogMovie, error) {
	if err := adapter.navigate(bookingMovieURL); err != nil {
		return nil, fmt.Errorf("open CGV movie catalog: %w", err)
	}
	if err := adapter.wait(1200 * time.Millisecond); err != nil {
		return nil, err
	}
	var movies []CatalogMovie
	if err := adapter.evaluate(`(() => {
		const suffix = ' 포스터';
		const movies = new Map();
		for (const image of window.__cinekoQueryAll('img[alt]')) {
			const label = image.getAttribute('alt') || '';
			if (!label.endsWith(suffix)) continue;
			const title = label.slice(0, -suffix.length).trim();
			if (title && !movies.has(title)) movies.set(title, {
				sourceKey: title,
				title,
				posterURL: image.currentSrc || image.src || '',
			});
		}
		return [...movies.values()];
	})()`, &movies); err != nil {
		return nil, fmt.Errorf("extract CGV movie catalog: %w", err)
	}
	if len(movies) == 0 {
		return nil, fmt.Errorf("%w: movie catalog is empty", ErrUIContractChanged)
	}
	sort.Slice(movies, func(left, right int) bool { return movies[left].Title < movies[right].Title })
	return movies, nil
}

func (adapter *Adapter) captureTheaters(ctx context.Context) ([]CatalogTheater, error) {
	regions, err := adapter.openTheaterCatalog()
	if err != nil {
		return nil, err
	}
	theaters := make([]CatalogTheater, 0)
	for _, region := range regions {
		values, err := adapter.captureRegionTheaters(ctx, region)
		if err != nil {
			return nil, err
		}
		theaters = append(theaters, values...)
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

func (adapter *Adapter) openTheaterCatalog() ([]catalogRegion, error) {
	if err := adapter.navigate(bookingCinemaURL); err != nil {
		return nil, fmt.Errorf("open CGV theater catalog: %w", err)
	}
	if err := adapter.wait(1200 * time.Millisecond); err != nil {
		return nil, err
	}
	opened, err := adapter.clickButtonExact("자주가는 CGV 목록 수정")
	if err != nil {
		return nil, err
	}
	if !opened {
		return nil, fmt.Errorf("%w: theater picker button not found", ErrUIContractChanged)
	}
	if err := adapter.wait(200 * time.Millisecond); err != nil {
		return nil, err
	}
	return adapter.catalogRegions()
}

func (adapter *Adapter) captureRegionTheaters(
	ctx context.Context,
	region catalogRegion,
) ([]CatalogTheater, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clicked, err := adapter.clickButtonPrefix(region.Name + "(")
	if err != nil {
		return nil, err
	}
	if !clicked {
		return nil, fmt.Errorf("%w: theater region %q not found", ErrUIContractChanged, region.Name)
	}
	if err := adapter.wait(150 * time.Millisecond); err != nil {
		return nil, err
	}
	names, err := adapter.waitForCatalogTheaters(region.Count)
	if err != nil {
		return nil, err
	}
	theaters := make([]CatalogTheater, 0, len(names))
	for _, name := range names {
		theaters = append(theaters, CatalogTheater{
			SourceKey: region.Name + "/" + name, Region: region.Name, Name: name,
		})
	}
	return theaters, nil
}

type catalogRegion struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (adapter *Adapter) catalogRegions() ([]catalogRegion, error) {
	var regions []catalogRegion
	if err := adapter.evaluate(`(() => {
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		return window.__cinekoQueryAll('button')
			.map(button => normalize(button.innerText || button.textContent))
			.map(label => label.match(/^(.+)\((\d+)\)$/))
			.filter(Boolean)
			.map(match => ({name: match[1].trim(), count: Number(match[2])}));
	})()`, &regions); err != nil {
		return nil, fmt.Errorf("extract CGV theater regions: %w", err)
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("%w: theater regions are empty", ErrUIContractChanged)
	}
	return regions, nil
}

func (adapter *Adapter) waitForCatalogTheaters(expected int) ([]string, error) {
	deadline := time.Now().Add(5 * time.Second)
	var latest []string
	for time.Now().Before(deadline) {
		names, err := adapter.catalogTheaterNames()
		if err != nil {
			return nil, err
		}
		latest = names
		if len(names) == expected {
			return names, nil
		}
		if err := adapter.wait(100 * time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf(
		"%w: expected %d theaters, observed %d",
		ErrUIContractChanged, expected, len(latest),
	)
}

func (adapter *Adapter) catalogTheaterNames() ([]string, error) {
	var names []string
	if err := adapter.evaluate(`(() => {
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const regionPattern = /^.+\(\d+\)$/;
		const search = window.__cinekoQueryAll('input')
			.find(input => (input.getAttribute('placeholder') || '').includes('극장명'));
		let dialog = search;
		while (dialog && dialog !== document.body) {
			const labels = window.__cinekoQueryAll('button', dialog)
				.map(button => normalize(button.innerText || button.textContent)).filter(Boolean);
			const regionCount = labels.filter(label => regionPattern.test(label)).length;
			if (regionCount >= 5 && labels.length >= regionCount + 3) break;
			dialog = dialog.parentElement;
		}
		if (!dialog) return [];
		const excluded = /^(지역별|특별관|추천|검색하기|자주가는 CGV 설정|닫기)$/;
		const labels = window.__cinekoQueryAll('button', dialog)
			.map(button => normalize(button.innerText || button.textContent))
			.filter(label => label && !excluded.test(label) && !regionPattern.test(label));
		return [...new Set(labels)];
	})()`, &names); err != nil {
		return nil, fmt.Errorf("extract CGV theater names: %w", err)
	}
	return names, nil
}
