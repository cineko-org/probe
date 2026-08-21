package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	probepb "github.com/cineko-org/contracts/gen/go/cineko/probe"
	"github.com/cineko-org/probe/v2/internal/bootstrap"
)

const maxBootstrapTicketBytes = 16 << 10

type CredentialSource interface {
	Credential(context.Context) (string, error)
}

type ClientCredentialConfig struct {
	PublicKeyFiles string
	Issuer         string
	Audience       string
	ClockSkew      time.Duration
	Registration   *probepb.RegisterRequest
}

// NewClientCredentialSource verifies every short-lived Central bootstrap
// ticket before the embedded Probe presents it. The Client needs only the
// public key files installed by its Launcher.
func NewClientCredentialSource(
	source CredentialSource,
	config ClientCredentialConfig,
) (CredentialSource, error) {
	keys, err := bootstrap.LoadPublicKeyFiles(config.PublicKeyFiles)
	if err != nil {
		return nil, fmt.Errorf("load Probe bootstrap public keys: %w", err)
	}
	verifier, err := bootstrap.NewVerifier(config.Issuer, config.Audience, keys, config.ClockSkew)
	if err != nil {
		return nil, err
	}
	return newVerifyingCredentialSource(source, verifier, config.Registration)
}

type VerifyingCredentialSource struct {
	source       CredentialSource
	verifier     *bootstrap.Verifier
	registration *probepb.RegisterRequest
	clock        func() time.Time
}

func newVerifyingCredentialSource(
	source CredentialSource,
	verifier *bootstrap.Verifier,
	registration *probepb.RegisterRequest,
) (*VerifyingCredentialSource, error) {
	if source == nil || verifier == nil || registration == nil || registration.GetKind().GetClient() == nil {
		return nil, errors.New("client Probe credential verifier configuration is invalid")
	}
	return &VerifyingCredentialSource{
		source: source, verifier: verifier, registration: registration, clock: time.Now,
	}, nil
}

func (source *VerifyingCredentialSource) Credential(ctx context.Context) (string, error) {
	ticket, err := source.source.Credential(ctx)
	if err != nil {
		return "", err
	}
	if _, err := source.verifier.Authorize(ctx, source.registration, ticket, source.clock().UTC()); err != nil {
		return "", errors.New("client Probe bootstrap ticket verification failed")
	}
	return ticket, nil
}

type StaticCredential string

func (credential StaticCredential) Credential(context.Context) (string, error) {
	value := strings.TrimSpace(string(credential))
	if value == "" {
		return "", errors.New("probe enrollment credential is empty")
	}
	return value, nil
}

// LineCredentialSource reads one short-lived bootstrap ticket per registration
// from a Launcher-owned anonymous pipe. The ticket never appears in argv,
// environment variables, files, or logs.
type LineCredentialSource struct {
	mu     sync.Mutex
	reader *bufio.Reader
}

func NewLineCredentialSource(reader io.Reader) (*LineCredentialSource, error) {
	if reader == nil {
		return nil, errors.New("client Probe bootstrap pipe is required")
	}
	return &LineCredentialSource{reader: bufio.NewReaderSize(reader, maxBootstrapTicketBytes+1)}, nil
}

func (source *LineCredentialSource) Credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	value, err := source.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read Client Probe bootstrap ticket: %w", err)
	}
	if len(value) > maxBootstrapTicketBytes {
		return "", errors.New("client Probe bootstrap ticket exceeds size limit")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("client Probe bootstrap pipe closed without a ticket")
	}
	return value, nil
}
