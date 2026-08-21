package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const ProviderCGV = "cgv"

// CatalogID derives one provider-scoped identifier from a provider source key.
// Every reporter uses this function so the same real-world entity converges on
// one Central row without trusting a Client-generated opaque identifier.
func CatalogID(providerID, kind, sourceKey string) string {
	normalized := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(providerID),
		strings.TrimSpace(kind),
		strings.TrimSpace(sourceKey),
	}, "\x00"))
	digest := sha256.Sum256([]byte(normalized))
	return strings.TrimSpace(kind) + "_" + hex.EncodeToString(digest[:16])
}

func SeatMapVersionID(auditoriumID, layoutHash string) string {
	return CatalogID("catalog", "seat-map", strings.TrimSpace(auditoriumID)+"\x00"+strings.TrimSpace(layoutHash))
}

// SeatID derives the stable identity used for the same physical seat across
// independent Probe and Client observations of an auditorium layout.
func SeatID(auditoriumID, label string) string {
	return CatalogID("catalog", "seat", strings.TrimSpace(auditoriumID)+"\x00"+strings.ToUpper(strings.TrimSpace(label)))
}
