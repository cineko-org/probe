package browser

import (
	"context"
	"errors"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	"os"
	"path/filepath"
	"testing"
)

func TestFactoryRequiresEgressManager(t *testing.T) {
	t.Parallel()
	if _, err := New(cgv.DefaultBrowserConfig(), nil); err == nil {
		t.Fatal("New() error = nil")
	}
}

func TestFactoryUsesThreeIsolatedSlotsAndStableSessionProfiles(t *testing.T) {
	t.Parallel()
	manager, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	config := cgv.DefaultBrowserConfig()
	config.ProfileDir = filepath.Join(t.TempDir(), "profiles")
	if err := os.MkdirAll(config.ProfileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ProfileDir, "authenticated-cookie"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	factory, err := New(config, manager)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	if got := cap(factory.slots); got != 3 {
		t.Fatalf("browser capacity = %d", got)
	}
	if got := cap(factory.sessions); got != 1 {
		t.Fatalf("session capacity = %d", got)
	}
	first, cleanup, err := factory.profileForTask(Task{Purpose: egress.PurposeScan}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("scan profile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first, "authenticated-cookie")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scan inherited account profile state: %v", err)
	}
	cleanup()
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scan profile was not removed: %v", err)
	}
	sessionA, _, _ := factory.profileForTask(Task{Purpose: egress.PurposeSession, SessionKey: "monitor-a"}, 0)
	sessionAAfterRestart, _, _ := factory.profileForTask(Task{Purpose: egress.PurposeSession, SessionKey: "monitor-a"}, 2)
	sessionB, _, _ := factory.profileForTask(Task{Purpose: egress.PurposeSession, SessionKey: "monitor-b"}, 0)
	if sessionA != sessionAAfterRestart || sessionA != sessionB || sessionA != config.ProfileDir {
		t.Fatalf("session profiles = %q, %q, %q", sessionA, sessionAAfterRestart, sessionB)
	}
}

func TestTaskBrowserIdentityPolicy(t *testing.T) {
	t.Parallel()
	base := cgv.DefaultBrowserConfig()
	session := browserConfigForTask(base, Task{Purpose: egress.PurposeSession, Headless: true})
	if !session.RestoreSession || session.BlockResources || session.UserAgentMode != cgv.UserAgentSession || session.Headless || !session.StartMinimized {
		t.Fatalf("session browser config = %+v", session)
	}
	scan := browserConfigForTask(base, Task{Purpose: egress.PurposeScan, Locale: "ko-KR", TimeZone: "Asia/Seoul"})
	if scan.RestoreSession || !scan.BlockResources || scan.UserAgentMode != cgv.UserAgentRandomizedScan ||
		scan.Locale != "ko-KR" || scan.TimeZone != "Asia/Seoul" {
		t.Fatalf("scan browser config = %+v", scan)
	}
}

func TestSessionLeaseStaysFixedAcrossBrowserRestarts(t *testing.T) {
	t.Parallel()
	manager, err := egress.New(egress.Config{Proxies: []egress.Proxy{{Server: "http://proxy.test:8080"}}})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := New(cgv.DefaultBrowserConfig(), manager)
	if err != nil {
		t.Fatal(err)
	}
	first, shared, err := factory.leaseForTask(context.Background(), manager, Task{Purpose: egress.PurposeSession})
	if err != nil || !shared {
		t.Fatalf("first session lease = %p, %t, %v", first, shared, err)
	}
	second, shared, err := factory.leaseForTask(context.Background(), manager, Task{Purpose: egress.PurposeSession})
	if err != nil || !shared || first != second {
		t.Fatalf("second session lease = %p, %t, %v; first = %p", second, shared, err, first)
	}
	scan, shared, err := factory.leaseForTask(context.Background(), manager, Task{
		Purpose: egress.PurposeScan, EgressPolicyID: egress.PolicyScanDefault,
	})
	if err != nil || shared || scan == first {
		t.Fatalf("scan lease = %p, %t, %v", scan, shared, err)
	}
	_ = scan.Close()
	factory.Close()
	if context.Cause(first.Context()) == nil {
		t.Fatal("factory close left the session proxy lease active")
	}
}

func TestScanLeaseUsesAssignmentEgressPolicy(t *testing.T) {
	t.Parallel()
	manager, err := egress.New(egress.Config{ScanProxies: []egress.Proxy{{Server: "http://policy-proxy.test:8080"}}})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := New(cgv.DefaultBrowserConfig(), manager)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()

	lease, shared, err := factory.leaseForTask(context.Background(), manager, Task{
		Purpose: egress.PurposeScan, EgressPolicyID: egress.PolicyScanDefault,
	})
	if err != nil || shared {
		t.Fatalf("policy scan lease = %p, %t, %v", lease, shared, err)
	}
	defer func() { _ = lease.Close() }()
	if proxy := lease.Proxy(); proxy == nil || proxy.Server != "http://policy-proxy.test:8080" {
		t.Fatalf("policy scan proxy = %+v", proxy)
	}
	if _, _, err := factory.leaseForTask(context.Background(), manager, Task{
		Purpose: egress.PurposeScan, EgressPolicyID: "unknown",
	}); !errors.Is(err, egress.ErrUnknownPolicy) {
		t.Fatalf("unknown policy error = %v", err)
	}
}

func TestClosedFactoryRejectsTasksWithoutStartingPlaywright(t *testing.T) {
	t.Parallel()
	manager, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := New(cgv.DefaultBrowserConfig(), manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := factory.Preflight(nil); err == nil { //nolint:staticcheck // verifies the nil boundary
		t.Fatal("Preflight(nil) error = nil")
	}
	factory.Close()
	if err := factory.Preflight(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Preflight() error = %v", err)
	}
	if _, err := factory.Open(context.Background(), Task{Purpose: egress.PurposeSession}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := factory.Open(nil, Task{Purpose: egress.PurposeSession}); err == nil { //nolint:staticcheck // verifies the nil boundary
		t.Fatal("Open(nil) error = nil")
	}
	factory.Close()
	if err := factory.ConfigureEgress(egress.Config{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("ConfigureEgress() error = %v", err)
	}
}

func TestFactoryReconfiguresFutureEgress(t *testing.T) {
	t.Parallel()
	manager, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := New(cgv.DefaultBrowserConfig(), manager)
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	if err := factory.ConfigureEgress(egress.Config{SoxyURL: "https://soxy.test"}); err == nil {
		t.Fatal("ConfigureEgress(invalid) error = nil")
	}
	if err := factory.ConfigureEgress(egress.Config{}); err != nil {
		t.Fatalf("ConfigureEgress(direct) error = %v", err)
	}
	configured, err := factory.currentEgress()
	if err != nil || configured == manager {
		t.Fatalf("currentEgress() = %p, %v", configured, err)
	}
}
