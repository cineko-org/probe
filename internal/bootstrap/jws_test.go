package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
	central "github.com/cineko-org/probe/v2/internal/protocol"
)

func TestBootstrapTicketRoundTripAndAuthorization(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC)
	key := testPrivateKey(t)
	signer, err := NewSigner("cineko-central", "cineko-probe", "key-2026", key)
	if err != nil {
		t.Fatal(err)
	}
	signer.clock = func() time.Time { return now }
	claims := validClaims()
	token, err := signer.Issue(claims, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(token, ".")) != 3 {
		t.Fatalf("ticket = %q", token)
	}
	verifier, err := NewVerifier(
		"cineko-central", "cineko-probe", map[string]*ecdsa.PublicKey{"key-2026": &key.PublicKey}, 5*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(token, now)
	if err != nil || verified.UserID != claims.UserID || verified.ExpiresAt != now.Add(time.Minute).Unix() {
		t.Fatalf("verified claims = %+v, error = %v", verified, err)
	}
	registration := central.RegisterProbeRequest{
		InstallationID: claims.InstallationID, Kind: "client", Capabilities: []string{contracts.CapabilityCGVScheduleCapture},
		MaxConcurrency: 1, Runtime: claims.Runtime,
	}
	authorization, err := verifier.Authorize(context.Background(), registration, token, now)
	if err != nil || authorization.OwnerUserID != claims.UserID || authorization.DeviceID != claims.DeviceID ||
		authorization.TicketID != claims.TicketID || !authorization.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("authorization = %+v, error = %v", authorization, err)
	}
}

func TestBootstrapTicketRejectsTamperingAndInvalidBindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC)
	key := testPrivateKey(t)
	signer, err := NewSigner("issuer", "audience", "key", key)
	if err != nil {
		t.Fatal(err)
	}
	signer.clock = func() time.Time { return now }
	claims := validClaims()
	token, err := signer.Issue(claims, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier("issuer", "audience", map[string]*ecdsa.PublicKey{"key": &key.PublicKey}, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	headerBytes, _ := json.Marshal(header{Algorithm: "none", KeyID: "key", Type: tokenType})
	unknownHeader, _ := json.Marshal(header{Algorithm: algorithm, KeyID: "unknown", Type: tokenType})
	extraHeader := rawEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"key","typ":"Cineko-Probe-Bootstrap","extra":true}`))
	highSignature := highSSignature(t, parts[2], key.Curve.Params().N)
	cases := []string{
		"", "one.two", "..", "!." + parts[1] + "." + parts[2],
		rawEncoding.EncodeToString(headerBytes) + "." + parts[1] + "." + parts[2],
		rawEncoding.EncodeToString(unknownHeader) + "." + parts[1] + "." + parts[2],
		extraHeader + "." + parts[1] + "." + parts[2],
		parts[0] + ".!." + parts[2], parts[0] + "." + parts[1] + ".!",
		parts[0] + "." + parts[1] + "." + rawEncoding.EncodeToString([]byte("short")),
		parts[0] + "." + parts[1] + "." + highSignature,
		parts[0] + "." + rawEncoding.EncodeToString([]byte(`{"bad":true}`)) + "." + parts[2],
	}
	for _, value := range cases {
		if _, err := verifier.Verify(value, now); !errors.Is(err, central.ErrUnauthorized) {
			t.Fatalf("tampered ticket %q error = %v", value, err)
		}
	}
	if _, err := verifier.Verify(token, now.Add(2*time.Minute)); !errors.Is(err, central.ErrUnauthorized) {
		t.Fatalf("expired ticket error = %v", err)
	}
	if _, err := verifier.Verify(token, now.Add(-2*time.Minute)); !errors.Is(err, central.ErrUnauthorized) {
		t.Fatalf("future ticket error = %v", err)
	}
	registration := central.RegisterProbeRequest{
		InstallationID: claims.InstallationID, Kind: "client", Capabilities: []string{contracts.CapabilityCGVScheduleCapture},
		MaxConcurrency: 1, Runtime: claims.Runtime,
	}
	mutations := []func(*central.RegisterProbeRequest){
		func(value *central.RegisterProbeRequest) { value.Kind = "container" },
		func(value *central.RegisterProbeRequest) { value.InstallationID = "other" },
		func(value *central.RegisterProbeRequest) { value.MaxConcurrency = 2 },
		func(value *central.RegisterProbeRequest) { value.Capabilities = []string{"other.v1"} },
		func(value *central.RegisterProbeRequest) { value.Runtime.Version = "2.0.0" },
	}
	for _, mutate := range mutations {
		value := registration
		mutate(&value)
		if _, err := verifier.Authorize(context.Background(), value, token, now); !errors.Is(err, central.ErrUnauthorized) {
			t.Fatalf("mismatched registration %+v error = %v", value, err)
		}
	}
}

func TestBootstrapCapabilitiesAreCanonicalAndExactlyBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	key := testPrivateKey(t)
	signer, err := NewSigner("issuer", "audience", "key", key)
	if err != nil {
		t.Fatal(err)
	}
	signer.clock = func() time.Time { return now }
	claims := validClaims()
	claims.Capabilities = []string{" cgv.schedule.capture.v2 "}
	token, err := signer.Issue(claims, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier("issuer", "audience", map[string]*ecdsa.PublicKey{"key": &key.PublicKey}, 0)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(token, now)
	if err != nil || !slices.Equal(verified.Capabilities, []string{contracts.CapabilityCGVScheduleCapture}) {
		t.Fatalf("verified capabilities = %v, error = %v", verified.Capabilities, err)
	}
	registration := central.RegisterProbeRequest{
		InstallationID: claims.InstallationID, Kind: "client",
		Capabilities:   []string{" cgv.schedule.capture.v2 "},
		MaxConcurrency: 1, Runtime: claims.Runtime,
	}
	if _, err := verifier.Authorize(context.Background(), registration, token, now); err != nil {
		t.Fatalf("normalized capability set rejected: %v", err)
	}
	registration.Capabilities = append(registration.Capabilities, "extra.v1")
	if _, err := verifier.Authorize(context.Background(), registration, token, now); !errors.Is(err, central.ErrUnauthorized) {
		t.Fatalf("expanded capability set error = %v", err)
	}
	claims.Capabilities = []string{contracts.CapabilityCGVScheduleCapture, " cgv.schedule.capture.v2 "}
	if _, err := signer.Issue(claims, time.Minute); !errors.Is(err, central.ErrUnauthorized) {
		t.Fatalf("duplicate capabilities error = %v", err)
	}
}

func TestBootstrapConfigurationAndSigningFailures(t *testing.T) {
	t.Parallel()
	key := testPrivateKey(t)
	if _, err := NewSigner("", "audience", "key", key); err == nil {
		t.Fatal("empty signer issuer accepted")
	}
	if _, err := NewSigner("issuer", "audience", "key", nil); err == nil {
		t.Fatal("nil signer key accepted")
	}
	if _, err := NewVerifier("", "audience", map[string]*ecdsa.PublicKey{"key": &key.PublicKey}, 0); err == nil {
		t.Fatal("empty verifier issuer accepted")
	}
	if _, err := NewVerifier("issuer", "audience", nil, 0); err == nil {
		t.Fatal("empty verifier keyring accepted")
	}
	if _, err := NewVerifier("issuer", "audience", map[string]*ecdsa.PublicKey{"": &key.PublicKey}, 0); err == nil {
		t.Fatal("empty verifier key id accepted")
	}
	if _, err := NewVerifier("issuer", "audience", map[string]*ecdsa.PublicKey{"key": nil}, 0); err == nil {
		t.Fatal("nil verifier key accepted")
	}
	if _, err := NewVerifier(
		"issuer", "audience", map[string]*ecdsa.PublicKey{"key": &key.PublicKey}, 2*time.Minute,
	); err == nil {
		t.Fatal("excessive clock skew accepted")
	}
	signer, err := NewSigner("issuer", "audience", "key", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Issue(validClaims(), 0); err == nil {
		t.Fatal("zero ticket lifetime accepted")
	}
	invalid := validClaims()
	invalid.UserID = ""
	if _, err := signer.Issue(invalid, time.Minute); !errors.Is(err, central.ErrUnauthorized) {
		t.Fatalf("invalid ticket claims error = %v", err)
	}
	signer.sign = func(io.Reader, *ecdsa.PrivateKey, []byte) (*big.Int, *big.Int, error) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	if _, err := signer.Issue(validClaims(), time.Minute); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("signature random error = %v", err)
	}
}

func TestBootstrapPEMAndKeyring(t *testing.T) {
	t.Parallel()
	key := testPrivateKey(t)
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if parsed, err := ParsePrivateKeyPEM(privatePEM); err != nil || !parsed.Equal(key) {
		t.Fatalf("private key parse error = %v", err)
	}
	if parsed, err := ParsePublicKeyPEM(publicPEM); err != nil || !parsed.Equal(&key.PublicKey) {
		t.Fatalf("public key parse error = %v", err)
	}
	directory := t.TempDir()
	publicPath := filepath.Join(directory, "public.pem")
	if err := os.WriteFile(publicPath, publicPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadPublicKeyFiles("current=" + publicPath)
	if err != nil || !keys["current"].Equal(&key.PublicKey) {
		t.Fatalf("loaded keys = %+v, error = %v", keys, err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaPrivateDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	rsaPublicDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	invalidPEM := []byte("not pem")
	for _, value := range [][]byte{
		invalidPEM,
		append(append([]byte(nil), privatePEM...), []byte("trailing")...),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaPrivateDER}),
	} {
		if _, err := ParsePrivateKeyPEM(value); err == nil {
			t.Fatalf("invalid private PEM accepted: %q", value)
		}
	}
	for _, value := range [][]byte{
		invalidPEM,
		append(append([]byte(nil), publicPEM...), []byte("trailing")...),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("invalid")}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaPublicDER}),
	} {
		if _, err := ParsePublicKeyPEM(value); err == nil {
			t.Fatalf("invalid public PEM accepted: %q", value)
		}
	}
	badPath := filepath.Join(directory, "bad.pem")
	if err := os.WriteFile(badPath, invalidPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{
		"", "missing-separator", "key=relative.pem", "key=" + filepath.Join(directory, "missing.pem"),
		"key=" + badPath, "key=" + publicPath + ",key=" + publicPath,
	} {
		if _, err := LoadPublicKeyFiles(spec); err == nil {
			t.Fatalf("invalid key spec %q accepted", spec)
		}
	}
}

func TestBootstrapHelpers(t *testing.T) {
	t.Parallel()
	var value map[string]bool
	if err := decodeStrict([]byte(`{"ok":true}`), &value); err != nil || !value["ok"] {
		t.Fatalf("strict decode = %+v, %v", value, err)
	}
	if err := decodeStrict([]byte(`{"ok":true} {}`), &value); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if err := decodeStrict([]byte(`{`), &value); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := decodeTokenClaims("!"); err == nil {
		t.Fatal("invalid claim encoding accepted")
	}
	if _, err := decodeTokenClaims(rawEncoding.EncodeToString([]byte(`{`))); err == nil {
		t.Fatal("invalid claim JSON accepted")
	}
	order := elliptic.P256().Params().N
	high := new(big.Int).Sub(order, big.NewInt(1))
	if isLowS(order, nil) || isLowS(order, big.NewInt(0)) || isLowS(order, order) || isLowS(order, high) {
		t.Fatal("invalid or high S accepted")
	}
	if got := normalizeLowS(order, high); !isLowS(order, got) {
		t.Fatalf("normalized S = %v", got)
	}
	if got := normalizeLowS(order, big.NewInt(1)); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("low S changed to %v", got)
	}
	for _, capabilities := range [][]string{nil, {""}, {" duplicate.v1 ", "duplicate.v1"}} {
		if _, valid := normalizeCapabilities(capabilities); valid {
			t.Fatalf("invalid capabilities accepted: %q", capabilities)
		}
	}
}

func validClaims() Claims {
	return Claims{
		UserID: "user_01", TicketID: "ticket_01", InstallationID: "install_01", DeviceID: "device_01",
		Kind: "client", Capabilities: []string{contracts.CapabilityCGVScheduleCapture}, MaxConcurrency: 1,
		Runtime: central.Runtime{
			Version: "1.0.0", Protocol: central.ProtocolVersion, BrowserRevision: "1228",
			Platform: "darwin", Arch: "arm64",
		},
	}
}

func testPrivateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func highSSignature(t *testing.T, encoded string, order *big.Int) string {
	t.Helper()
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	s := new(big.Int).SetBytes(value[32:])
	high := new(big.Int).Sub(order, s)
	high.FillBytes(value[32:])
	return base64.RawURLEncoding.EncodeToString(value)
}
