package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	servicepb "github.com/cineko-org/contracts/v3/gen/go/cineko/service"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	usage = `usage:
  releasecontract render VERSION BROWSER_REVISION IMAGE IMAGE_DIGEST PUBLISHED_AT
  releasecontract publish CENTRAL_URL VERSION BROWSER_REVISION IMAGE IMAGE_DIGEST PUBLISHED_AT`
	maxPublishAttempts = 4
	maxResponseBytes   = 1 << 20
)

var (
	semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	digitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "render":
		request, err := releaseRequest(args[1:])
		if err != nil {
			return err
		}
		payload, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(request)
		if err != nil {
			return fmt.Errorf("encode generated Probe release request: %w", err)
		}
		_, err = fmt.Fprintf(os.Stdout, "%s\n", payload)
		return err
	case "publish":
		return publishFromArgs(args[1:])
	default:
		return errors.New(usage)
	}
}

func publishFromArgs(args []string) error {
	if len(args) != 6 {
		return errors.New(usage)
	}
	centralURL := strings.TrimSuffix(args[0], "/")
	parsed, err := url.ParseRequestURI(centralURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("central URL must be HTTPS")
	}
	token := strings.TrimSpace(os.Getenv("CINEKO_RELEASE_PUBLISH_TOKEN"))
	if token == "" {
		return errors.New("release publisher token is required")
	}
	request, err := releaseRequest(args[1:])
	if err != nil {
		return err
	}
	payload, err := protojson.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode generated Probe release request: %w", err)
	}
	return publishProbeRelease(
		context.Background(), &http.Client{Timeout: 30 * time.Second}, time.Sleep,
		centralURL+"/v1/release-registry/probe", token, payload,
	)
}

func releaseRequest(args []string) (*servicepb.PublishProbeRequest, error) {
	if len(args) != 5 {
		return nil, errors.New(usage)
	}
	version, browserRevision, image, digest := args[0], args[1], args[2], args[3]
	if !semverPattern.MatchString(version) {
		return nil, errors.New("VERSION must use semantic versioning without a v prefix")
	}
	if !digitsPattern.MatchString(browserRevision) {
		return nil, errors.New("BROWSER_REVISION must be a nonnegative integer")
	}
	if !validImageRepository(image) {
		return nil, errors.New("IMAGE must be an untagged, digest-free repository")
	}
	if !digestPattern.MatchString(digest) {
		return nil, errors.New("IMAGE_DIGEST must be a lowercase sha256 OCI digest")
	}
	publishedAt, err := time.Parse(time.RFC3339, args[4])
	if err != nil || publishedAt.Location() != time.UTC {
		return nil, errors.New("PUBLISHED_AT must be a valid RFC3339 UTC timestamp")
	}
	channel := "stable"
	release := releasepb.ProbeRelease_builder{
		Channel: &channel, Version: &version, BrowserRevision: &browserRevision,
		Image: &image, ImageDigest: &digest, PublishedAt: timestamppb.New(publishedAt),
	}.Build()
	set := releasepb.ProbeReleaseSet_builder{Releases: []*releasepb.ProbeRelease{release}}.Build()
	request := servicepb.PublishProbeRequest_builder{ReleaseSet: set}.Build()
	if err := protovalidate.Validate(request); err != nil {
		return nil, fmt.Errorf("validate generated Probe publish request: %w", err)
	}
	return request, nil
}

func validImageRepository(image string) bool {
	final := image[strings.LastIndex(image, "/")+1:]
	return strings.TrimSpace(image) == image && image != "" && !strings.ContainsAny(image, " \t\r\n@") &&
		!strings.Contains(image, "://") && strings.Contains(image, "/") && !strings.Contains(final, ":")
}

func publishProbeRelease(
	ctx context.Context,
	client *http.Client,
	sleep func(time.Duration),
	endpoint string,
	token string,
	payload []byte,
) error {
	for attempt := 1; attempt <= maxPublishAttempts; attempt++ {
		// #nosec G107,G704 -- publishFromArgs validates the operator-supplied Central HTTPS origin.
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create Probe release request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request) // #nosec G704 -- endpoint was validated by publishFromArgs.
		if err != nil {
			if attempt == maxPublishAttempts {
				return fmt.Errorf("central Probe release registration failed after %d network attempts: %w", attempt, err)
			}
			sleep(publishBackoff(attempt))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read Central Probe release response: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close Central Probe release response: %w", closeErr)
		}
		if len(body) > maxResponseBytes {
			return errors.New("central Probe release response exceeds size limit")
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return acceptProbePublish(response, body)
		}
		failure := centralFailure(response.StatusCode, body)
		if response.StatusCode < http.StatusInternalServerError || attempt == maxPublishAttempts {
			return failure
		}
		sleep(publishBackoff(attempt))
	}
	return errors.New("central Probe release registration exhausted all attempts")
}

func acceptProbePublish(response *http.Response, body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("central returned an empty Probe publish response")
	}
	output := &servicepb.PublishProbeResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode generated Probe publish response: %w", err)
	}
	if err := protovalidate.Validate(output); err != nil {
		return fmt.Errorf("validate generated Probe publish response: %w", err)
	}
	generation, err := positiveGeneration(response.Header.Get("X-Cineko-Release-Generation"))
	if err != nil {
		return err
	}
	fmt.Printf("registered Probe release generation %d\n", generation)
	return nil
}

func centralFailure(status int, payload []byte) error {
	response := &commonpb.APIErrorResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, response); err == nil && response.GetError() != nil {
		return fmt.Errorf(
			"central Probe release registration failed with HTTP %d: %s: %s",
			status, response.GetError().GetCode(), response.GetError().GetMessage(),
		)
	}
	return fmt.Errorf("central Probe release registration failed with HTTP %d", status)
}

func positiveGeneration(value string) (int64, error) {
	generation, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || generation <= 0 {
		return 0, errors.New("central returned an invalid release generation header")
	}
	return generation, nil
}

func publishBackoff(attempt int) time.Duration {
	return time.Second * time.Duration(1<<(attempt-1))
}
