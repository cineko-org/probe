package cgv

import (
	"strings"
	"testing"

	contracts "github.com/cineko-org/contracts/v3"
)

func TestParseTheaterCatalogResponseUsesSiteNumber(t *testing.T) {
	t.Parallel()
	rows, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":[{"siteNo":"0056","siteNm":"용산아이파크몰","regnGrpNm":"서울"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SiteNo != "0056" || rows[0].SiteName != "용산아이파크몰" || rows[0].Region != "서울" {
		t.Fatalf("theater rows = %+v", rows)
	}
	if got := contracts.CatalogID(contracts.ProviderCGV, "theater", rows[0].SiteNo); got == contracts.CatalogID(contracts.ProviderCGV, "theater", rows[0].SiteName) {
		t.Fatal("display name unexpectedly equals provider identity")
	}
}

func TestParseTheaterCatalogResponseFailsClosed(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"siteNo", "siteNm"} {
		payload := `{"statusCode":0,"data":[{"siteNo":"0056","siteNm":"용산아이파크몰"}]}`
		payload = strings.Replace(payload, `"`+field+`":"`+map[string]string{"siteNo": "0056", "siteNm": "용산아이파크몰"}[field]+`"`, "", 1)
		if _, err := parseTheaterCatalogResponse([]byte(payload)); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	if _, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":null}`)); err == nil {
		t.Fatal("null theater data accepted")
	}
}

func TestTheaterCatalogResponsePathIsExact(t *testing.T) {
	t.Parallel()
	if path, ok := providerResponsePath("https://cgv.co.kr/api/v1/content/site/searchAllRegionAndSite?coCd=A420"); !ok || path != theaterCatalogResponsePath {
		t.Fatalf("theater response path = %q, %t", path, ok)
	}
	if _, ok := providerResponsePath("https://cgv.co.kr/api/v1/content/site/searchAllRegionAndSiteFake"); ok {
		t.Fatal("untrusted theater endpoint accepted")
	}
}
