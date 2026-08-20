package cgv

import (
	"fmt"
	"strings"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

func canonicalProviderDate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("provider date is empty")
	}
	for _, layout := range []string{"20060102", time.DateOnly} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("provider date %q is not YYYYMMDD or YYYY-MM-DD", raw)
}

func movieSourceKey(movieNo string) string { return strings.TrimSpace(movieNo) }

func auditoriumSourceKey(siteNo, auditoriumNo string) string {
	return strings.Join([]string{strings.TrimSpace(siteNo), strings.TrimSpace(auditoriumNo)}, "/")
}

func showtimeSourceKey(siteNo, date, auditoriumNo, sequence string) string {
	return strings.Join([]string{
		strings.TrimSpace(siteNo), strings.TrimSpace(date), strings.TrimSpace(auditoriumNo), strings.TrimSpace(sequence),
	}, "/")
}

func providerCatalogID(kind, sourceKey string) string {
	return contracts.CatalogID(contracts.ProviderCGV, kind, sourceKey)
}
