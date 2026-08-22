package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	servicepb "github.com/cineko-org/contracts/v3/gen/go/cineko/service"
	"google.golang.org/protobuf/encoding/protojson"
)

var validReleaseArgs = []string{
	"2.2.0", "1228", "registry.example/cineko/probe",
	"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"2026-08-19T10:00:00Z",
}

func TestReleaseRequestUsesGeneratedPublishProbeMessage(t *testing.T) {
	request, err := releaseRequest(validReleaseArgs)
	if err != nil {
		t.Fatal(err)
	}
	releases := request.GetReleaseSet().GetReleases()
	if len(releases) != 1 || releases[0].GetVersion() != "2.2.0" || releases[0].GetBrowserRevision() != "1228" {
		t.Fatalf("generated Probe release request = %+v", request)
	}
	payload, err := protojson.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &servicepb.PublishProbeRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, decoded); err != nil {
		t.Fatal(err)
	}

	invalid := [][]string{
		{"v2.2.0", "1228", validReleaseArgs[2], validReleaseArgs[3], validReleaseArgs[4]},
		{"2.2.0", "current", validReleaseArgs[2], validReleaseArgs[3], validReleaseArgs[4]},
		{"2.2.0", "1228", "probe", validReleaseArgs[3], validReleaseArgs[4]},
		{"2.2.0", "1228", validReleaseArgs[2], "sha256:no", validReleaseArgs[4]},
		{"2.2.0", "1228", validReleaseArgs[2], validReleaseArgs[3], "2026-08-19"},
	}
	for _, args := range invalid {
		if _, err := releaseRequest(args); err == nil {
			t.Fatalf("invalid release arguments accepted: %v", args)
		}
	}
}

func TestPublishProbeReleaseUsesGeneratedRequestAndResponse(t *testing.T) {
	input, err := releaseRequest(validReleaseArgs)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protojson.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer publisher" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		decoded := &servicepb.PublishProbeRequest{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, decoded); err != nil {
			t.Errorf("decode Probe publish request: %v", err)
		}
		writer.Header().Set("X-Cineko-Release-Generation", "19")
		_, _ = writer.Write([]byte("{}"))
	}))
	defer server.Close()
	if err := publishProbeRelease(t.Context(), server.Client(), func(time.Duration) {}, server.URL, "publisher", payload); err != nil {
		t.Fatal(err)
	}
}

func TestPublishProbeReleaseRejectsEmptyAndUnknownResponses(t *testing.T) {
	input, err := releaseRequest(validReleaseArgs)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protojson.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"empty": "", "unknown": `{"generation":"19"}`} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("X-Cineko-Release-Generation", "19")
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			err := publishProbeRelease(t.Context(), server.Client(), func(time.Duration) {}, server.URL, "publisher", payload)
			if err == nil || (!strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "decode")) {
				t.Fatalf("response error = %v", err)
			}
		})
	}
}
