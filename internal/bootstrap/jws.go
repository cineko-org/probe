package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
	central "github.com/cineko-org/probe/v2/internal/protocol"
)

const (
	algorithm = "ES256"
	tokenType = "Cineko-Probe-Bootstrap" // #nosec G101 -- public JOSE type marker, not a credential.
)

var rawEncoding = base64.RawURLEncoding

type Claims = contracts.ProbeBootstrapClaims

type header struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type Signer struct {
	issuer   string
	audience string
	keyID    string
	key      *ecdsa.PrivateKey
	clock    func() time.Time
	random   io.Reader
	sign     func(io.Reader, *ecdsa.PrivateKey, []byte) (*big.Int, *big.Int, error)
}

func NewSigner(issuer, audience, keyID string, key *ecdsa.PrivateKey) (*Signer, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" || strings.TrimSpace(keyID) == "" {
		return nil, errors.New("bootstrap signer issuer, audience and key id are required")
	}
	if !validPrivateKey(key) {
		return nil, errors.New("bootstrap signer requires an ECDSA P-256 private key")
	}
	return &Signer{
		issuer: issuer, audience: audience, keyID: keyID, key: key,
		clock: time.Now, random: rand.Reader, sign: ecdsa.Sign,
	}, nil
}

func (signer *Signer) Issue(claims Claims, lifetime time.Duration) (string, error) {
	if lifetime <= 0 {
		return "", errors.New("bootstrap ticket lifetime must be positive")
	}
	now := signer.clock().UTC().Truncate(time.Second)
	claims.Issuer = signer.issuer
	claims.Audience = signer.audience
	claims.IssuedAt = now.Unix()
	claims.NotBefore = now.Unix()
	claims.ExpiresAt = now.Add(lifetime).Unix()
	capabilities, validCapabilities := normalizeCapabilities(claims.Capabilities)
	if !validCapabilities {
		return "", central.ErrUnauthorized
	}
	claims.Capabilities = capabilities
	if err := validateClaims(claims, now, signer.issuer, signer.audience); err != nil {
		return "", err
	}
	// These concrete structs contain only JSON primitives, so encoding cannot fail.
	headerBytes, _ := json.Marshal(header{Algorithm: algorithm, KeyID: signer.keyID, Type: tokenType})
	payloadBytes, _ := json.Marshal(claims)
	signingInput := rawEncoding.EncodeToString(headerBytes) + "." + rawEncoding.EncodeToString(payloadBytes)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := signer.sign(signer.random, signer.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign bootstrap ticket: %w", err)
	}
	s = normalizeLowS(signer.key.Curve.Params().N, s)
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + rawEncoding.EncodeToString(signature), nil
}

type Verifier struct {
	issuer    string
	audience  string
	keys      map[string]*ecdsa.PublicKey
	clockSkew time.Duration
}

func NewVerifier(
	issuer string,
	audience string,
	keys map[string]*ecdsa.PublicKey,
	clockSkew time.Duration,
) (*Verifier, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("bootstrap verifier issuer and audience are required")
	}
	if clockSkew < 0 || clockSkew > time.Minute {
		return nil, errors.New("bootstrap verifier clock skew must be between zero and one minute")
	}
	keyring := make(map[string]*ecdsa.PublicKey, len(keys))
	for keyID, key := range keys {
		if strings.TrimSpace(keyID) == "" || !validPublicKey(key) {
			return nil, errors.New("bootstrap verifier keyring contains an invalid P-256 key")
		}
		keyring[keyID] = key
	}
	if len(keyring) == 0 {
		return nil, errors.New("bootstrap verifier requires at least one public key")
	}
	return &Verifier{issuer: issuer, audience: audience, keys: keyring, clockSkew: clockSkew}, nil
}

func (verifier *Verifier) Verify(token string, now time.Time) (Claims, error) {
	parts, key, err := verifier.parseTokenHeader(token)
	if err != nil || !verifyTokenSignature(parts, key) {
		return Claims{}, central.ErrUnauthorized
	}
	claims, err := decodeTokenClaims(parts[1])
	if err != nil || validateClaims(claims, now.UTC(), verifier.issuer, verifier.audience) != nil {
		return Claims{}, central.ErrUnauthorized
	}
	if now.Add(verifier.clockSkew).Before(time.Unix(claims.NotBefore, 0)) ||
		!now.Add(-verifier.clockSkew).Before(time.Unix(claims.ExpiresAt, 0)) {
		return Claims{}, central.ErrUnauthorized
	}
	return claims, nil
}

func (verifier *Verifier) parseTokenHeader(token string) ([]string, *ecdsa.PublicKey, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, nil, central.ErrUnauthorized
	}
	headerBytes, err := rawEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return nil, nil, central.ErrUnauthorized
	}
	var ticketHeader header
	if err := decodeStrict(headerBytes, &ticketHeader); err != nil ||
		ticketHeader.Algorithm != algorithm || ticketHeader.Type != tokenType {
		return nil, nil, central.ErrUnauthorized
	}
	key := verifier.keys[ticketHeader.KeyID]
	if key == nil {
		return nil, nil, central.ErrUnauthorized
	}
	return parts, key, nil
}

func verifyTokenSignature(parts []string, key *ecdsa.PublicKey) bool {
	signature, err := rawEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !isLowS(key.Curve.Params().N, s) {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(key, digest[:], r, s)
}

func decodeTokenClaims(payload string) (Claims, error) {
	payloadBytes, err := rawEncoding.Strict().DecodeString(payload)
	if err != nil {
		return Claims{}, err
	}
	var claims Claims
	if err := decodeStrict(payloadBytes, &claims); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func (verifier *Verifier) Authorize(
	_ context.Context,
	request central.RegisterProbeRequest,
	token string,
	now time.Time,
) (central.RegistrationAuthorization, error) {
	claims, err := verifier.Verify(token, now)
	capabilities, validCapabilities := normalizeCapabilities(request.Capabilities)
	if err != nil || claims.Kind != "client" || request.Kind != "client" ||
		claims.InstallationID != request.InstallationID || claims.MaxConcurrency != request.MaxConcurrency ||
		claims.Runtime != request.Runtime || !validCapabilities || !slices.Equal(claims.Capabilities, capabilities) {
		return central.RegistrationAuthorization{}, central.ErrUnauthorized
	}
	return central.RegistrationAuthorization{
		OwnerUserID: claims.UserID, DeviceID: claims.DeviceID, TicketID: claims.TicketID,
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
	}, nil
}

func ParsePrivateKeyPEM(contents []byte) (*ecdsa.PrivateKey, error) {
	return parseTypedPEM(
		contents, "PRIVATE KEY", "private", x509.ParsePKCS8PrivateKey,
		func(value any) (*ecdsa.PrivateKey, bool) {
			key, ok := value.(*ecdsa.PrivateKey)
			return key, ok && validPrivateKey(key)
		},
	)
}

func ParsePublicKeyPEM(contents []byte) (*ecdsa.PublicKey, error) {
	return parseTypedPEM(
		contents, "PUBLIC KEY", "public", x509.ParsePKIXPublicKey,
		func(value any) (*ecdsa.PublicKey, bool) {
			key, ok := value.(*ecdsa.PublicKey)
			return key, ok && validPublicKey(key)
		},
	)
}

func parseTypedPEM[T any](
	contents []byte,
	blockType string,
	description string,
	parser func([]byte) (any, error),
	convert func(any) (T, bool),
) (T, error) {
	var zero T
	der, err := decodeSinglePEM(contents, blockType)
	if err != nil {
		return zero, fmt.Errorf("bootstrap %s key: %w", description, err)
	}
	parsed, err := parser(der)
	if err != nil {
		return zero, fmt.Errorf("parse bootstrap %s key: %w", description, err)
	}
	key, ok := convert(parsed)
	if !ok {
		return zero, fmt.Errorf("bootstrap %s key must be ECDSA P-256", description)
	}
	return key, nil
}

func decodeSinglePEM(contents []byte, blockType string) ([]byte, error) {
	block, rest := pem.Decode(contents)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 || block.Type != blockType {
		return nil, fmt.Errorf("must contain one %s PEM block", blockType)
	}
	return block.Bytes, nil
}

func LoadPublicKeyFiles(spec string) (map[string]*ecdsa.PublicKey, error) {
	keys := make(map[string]*ecdsa.PublicKey)
	for _, entry := range strings.Split(strings.TrimSpace(spec), ",") {
		keyID, path, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found || strings.TrimSpace(keyID) == "" || strings.TrimSpace(path) == "" {
			return nil, errors.New("bootstrap public keys must use kid=/absolute/key.pem entries")
		}
		if !filepath.IsAbs(path) {
			return nil, errors.New("bootstrap public key paths must be absolute")
		}
		contents, err := os.ReadFile(path) // #nosec G304,G703 -- operator-configured absolute public-key path.
		if err != nil {
			return nil, fmt.Errorf("read bootstrap public key %q: %w", keyID, err)
		}
		key, err := ParsePublicKeyPEM(contents)
		if err != nil {
			return nil, fmt.Errorf("parse bootstrap public key %q: %w", keyID, err)
		}
		if _, duplicate := keys[keyID]; duplicate {
			return nil, fmt.Errorf("duplicate bootstrap key id %q", keyID)
		}
		keys[keyID] = key
	}
	return keys, nil
}

func decodeStrict(contents []byte, output any) error {
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON value")
	}
	return nil
}

func validateClaims(claims Claims, now time.Time, issuer, audience string) error {
	capabilities, validCapabilities := normalizeCapabilities(claims.Capabilities)
	if claims.Issuer != issuer || claims.Audience != audience || strings.TrimSpace(claims.UserID) == "" ||
		strings.TrimSpace(claims.TicketID) == "" || strings.TrimSpace(claims.InstallationID) == "" ||
		strings.TrimSpace(claims.DeviceID) == "" || claims.Kind != "client" || claims.MaxConcurrency != 1 ||
		!validCapabilities || !slices.Equal(claims.Capabilities, capabilities) ||
		claims.IssuedAt <= 0 || claims.NotBefore < claims.IssuedAt || claims.ExpiresAt <= claims.NotBefore ||
		time.Unix(claims.IssuedAt, 0).After(now.Add(time.Minute)) {
		return central.ErrUnauthorized
	}
	return nil
}

func normalizeCapabilities(values []string) ([]string, bool) {
	if len(values) == 0 {
		return nil, false
	}
	result := make([]string, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !contracts.IsSupportedCapability(value) {
			return nil, false
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, false
		}
		seen[value] = struct{}{}
		result[index] = value
	}
	slices.Sort(result)
	return result, true
}

func validPrivateKey(key *ecdsa.PrivateKey) bool {
	if key == nil || !validPublicKey(&key.PublicKey) {
		return false
	}
	_, err := key.ECDH()
	return err == nil
}

func validPublicKey(key *ecdsa.PublicKey) bool {
	if key == nil || key.Curve != elliptic.P256() {
		return false
	}
	_, err := key.ECDH()
	return err == nil
}

func normalizeLowS(order, value *big.Int) *big.Int {
	if isLowS(order, value) {
		return value
	}
	return new(big.Int).Sub(order, value)
}

func isLowS(order, value *big.Int) bool {
	if value == nil || value.Sign() <= 0 || value.Cmp(order) >= 0 {
		return false
	}
	return value.Cmp(new(big.Int).Rsh(new(big.Int).Set(order), 1)) <= 0
}
