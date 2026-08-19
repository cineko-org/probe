package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cineko-org/probe/v2/internal/bootstrap"
)

func TestCredentialSources(t *testing.T) {
	t.Parallel()
	if value, err := StaticCredential(" token ").Credential(context.Background()); err != nil || value != "token" {
		t.Fatalf("static credential = %q, %v", value, err)
	}
	if _, err := StaticCredential(" ").Credential(context.Background()); err == nil {
		t.Fatal("empty static credential accepted")
	}
	if _, err := NewLineCredentialSource(nil); err == nil {
		t.Fatal("nil bootstrap pipe accepted")
	}
	source, err := NewLineCredentialSource(strings.NewReader(" ticket-one \nsecond"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Credential(context.Background())
	if err != nil || first != "ticket-one" {
		t.Fatalf("first pipe credential = %q, %v", first, err)
	}
	second, err := source.Credential(context.Background())
	if err != nil || second != "second" {
		t.Fatalf("second pipe credential = %q, %v", second, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Credential(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled credential error = %v", err)
	}
	empty, _ := NewLineCredentialSource(strings.NewReader("\n"))
	if _, err := empty.Credential(context.Background()); err == nil {
		t.Fatal("empty pipe credential accepted")
	}
	broken, _ := NewLineCredentialSource(errorReader{})
	if _, err := broken.Credential(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("pipe read error = %v", err)
	}
	large, _ := NewLineCredentialSource(strings.NewReader(strings.Repeat("x", maxBootstrapTicketBytes+1) + "\n"))
	if _, err := large.Credential(context.Background()); err == nil {
		t.Fatal("oversized pipe credential accepted")
	}
}

func TestVerifyingCredentialSource(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := bootstrap.NewSigner("issuer", "audience", "key", key)
	if err != nil {
		t.Fatal(err)
	}
	registration := testRegistration()
	registration.Kind = "client"
	ticket, err := signer.Issue(bootstrap.Claims{
		UserID: "user", TicketID: "ticket", InstallationID: registration.InstallationID, DeviceID: "device",
		Kind: "client", Capabilities: registration.Capabilities, MaxConcurrency: 1, Runtime: registration.Runtime,
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := bootstrap.NewVerifier(
		"issuer", "audience", map[string]*ecdsa.PublicKey{"key": &key.PublicKey}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newVerifyingCredentialSource(nil, verifier, registration); err == nil {
		t.Fatal("nil delegated credential source accepted")
	}
	if _, err := newVerifyingCredentialSource(StaticCredential(ticket), nil, registration); err == nil {
		t.Fatal("nil ticket verifier accepted")
	}
	container := registration
	container.Kind = "container"
	if _, err := newVerifyingCredentialSource(StaticCredential(ticket), verifier, container); err == nil {
		t.Fatal("container registration accepted by client verifier")
	}
	source, err := newVerifyingCredentialSource(StaticCredential(ticket), verifier, registration)
	if err != nil {
		t.Fatal(err)
	}
	source.clock = func() time.Time { return now }
	if value, err := source.Credential(context.Background()); err != nil || value != ticket {
		t.Fatalf("verified credential = %q, %v", value, err)
	}
	invalid, _ := newVerifyingCredentialSource(StaticCredential("invalid"), verifier, registration)
	invalid.clock = func() time.Time { return now }
	if _, err := invalid.Credential(context.Background()); err == nil {
		t.Fatal("invalid bootstrap ticket accepted")
	}
	upstreamError := &credentialErrorSource{}
	failed, _ := newVerifyingCredentialSource(upstreamError, verifier, registration)
	if _, err := failed.Credential(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("upstream credential error = %v", err)
	}
}

func TestClientCredentialSourceLoadsPublicKeys(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	contents := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	keyPath := filepath.Join(t.TempDir(), "primary.pem")
	if err := os.WriteFile(keyPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	registration := testRegistration()
	registration.Kind = "client"
	config := ClientCredentialConfig{
		PublicKeyFiles: "primary=" + keyPath,
		Issuer:         "issuer",
		Audience:       "audience",
		Registration:   registration,
	}
	if _, err := NewClientCredentialSource(StaticCredential("ticket"), config); err != nil {
		t.Fatal(err)
	}
	config.PublicKeyFiles = "primary=" + filepath.Join(t.TempDir(), "missing.pem")
	if _, err := NewClientCredentialSource(StaticCredential("ticket"), config); err == nil {
		t.Fatal("missing public key file accepted")
	}
	config.PublicKeyFiles = "primary=" + keyPath
	config.Issuer = ""
	if _, err := NewClientCredentialSource(StaticCredential("ticket"), config); err == nil {
		t.Fatal("empty verifier issuer accepted")
	}
}

type credentialErrorSource struct{}

func (*credentialErrorSource) Credential(context.Context) (string, error) {
	return "", io.ErrUnexpectedEOF
}
