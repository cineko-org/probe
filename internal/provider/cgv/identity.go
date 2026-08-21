package cgv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const ProviderCGV = "cgv"

func CatalogID(providerID, kind, sourceKey string) string {
	normalized := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(providerID), strings.TrimSpace(kind), strings.TrimSpace(sourceKey),
	}, "\x00"))
	digest := sha256.Sum256([]byte(normalized))
	return strings.TrimSpace(kind) + "_" + hex.EncodeToString(digest[:16])
}

func SeatID(auditoriumID, label string) string {
	return CatalogID("catalog", "seat", strings.TrimSpace(auditoriumID)+"\x00"+strings.ToUpper(strings.TrimSpace(label)))
}

func SeatMapVersionID(auditoriumID, layoutHash string) string {
	return CatalogID("catalog", "seat-map", strings.TrimSpace(auditoriumID)+"\x00"+strings.TrimSpace(layoutHash))
}

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
	return CatalogID(ProviderCGV, kind, sourceKey)
}
