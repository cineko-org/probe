package cgv

import (
	"strings"
	"testing"
)

func TestParseTheaterCatalogResponseUsesSiteNumber(t *testing.T) {
	t.Parallel()
	rows, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":{"regionInfo":[{"comCdval":"01","comCdvalNm":"서울"}],"siteInfo":[{"regnGrpCd":"01","siteNo":"0056","siteNm":"용산아이파크몰"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SiteNo != "0056" || rows[0].SiteName != "용산아이파크몰" || rows[0].Region != "서울" {
		t.Fatalf("theater rows = %+v", rows)
	}
	if got := CatalogID(ProviderCGV, "theater", rows[0].SiteNo); got == CatalogID(ProviderCGV, "theater", rows[0].SiteName) {
		t.Fatal("display name unexpectedly equals provider identity")
	}
}

func TestParseTheaterCatalogResponseAcceptsOpaqueSiteNumbers(t *testing.T) {
	t.Parallel()

	rows, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":{"regionInfo":[{"comCdval":"01","comCdvalNm":"서울"}],"siteInfo":[{"regnGrpCd":"01","siteNo":"P001","siteNm":"씨네드쉐프 압구정"},{"regnGrpCd":"01","siteNo":"P004","siteNm":"씨네드쉐프 센텀"},{"regnGrpCd":"01","siteNo":"P013","siteNm":"씨네드쉐프 용산"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].SiteNo != "P001" || rows[1].SiteNo != "P004" || rows[2].SiteNo != "P013" {
		t.Fatalf("opaque theater rows = %+v", rows)
	}
}

func TestParseTheaterCatalogResponseFailsClosed(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"siteNo", "siteNm", "regnGrpCd"} {
		payload := `{"statusCode":0,"data":{"regionInfo":[{"comCdval":"01","comCdvalNm":"서울"}],"siteInfo":[{"regnGrpCd":"01","siteNo":"0056","siteNm":"용산아이파크몰"}]}}`
		payload = strings.Replace(payload, `"`+field+`":"`+map[string]string{"siteNo": "0056", "siteNm": "용산아이파크몰", "regnGrpCd": "01"}[field]+`"`, "", 1)
		if _, err := parseTheaterCatalogResponse([]byte(payload)); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	if _, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":null}`)); err == nil {
		t.Fatal("null theater data accepted")
	}
	if _, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":{"regionInfo":null,"siteInfo":[]}}`)); err == nil {
		t.Fatal("null theater regions accepted")
	}
	if _, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":{"regionInfo":[],"siteInfo":null}}`)); err == nil {
		t.Fatal("null theater sites accepted")
	}
	if _, err := parseTheaterCatalogResponse([]byte(`{"statusCode":0,"data":{"regionInfo":[{"comCdval":"01","comCdvalNm":"서울"}],"siteInfo":[{"regnGrpCd":"02","siteNo":"0056","siteNm":"용산아이파크몰"}]}}`)); err == nil {
		t.Fatal("unknown theater region accepted")
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
