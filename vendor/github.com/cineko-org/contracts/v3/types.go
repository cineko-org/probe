package contracts

import (
	"encoding/json"
	"time"
)

type Runtime struct {
	Version         string `json:"version"`
	Protocol        int    `json:"protocol"`
	BrowserRevision string `json:"browserRevision"`
	Platform        string `json:"platform"`
	Arch            string `json:"arch"`
}

type RegisterProbeRequest struct {
	InstallationID string   `json:"installationId"`
	Kind           string   `json:"kind"`
	NetworkHint    string   `json:"networkHint,omitempty"`
	Capabilities   []string `json:"capabilities"`
	MaxConcurrency int      `json:"maxConcurrency"`
	Runtime        Runtime  `json:"runtime"`
}

type RegisterProbeResponse struct {
	ProbeID                  string    `json:"probeId"`
	NetworkID                string    `json:"networkId"`
	AccessToken              string    `json:"accessToken"`
	TokenExpiresAt           time.Time `json:"tokenExpiresAt"`
	HeartbeatIntervalSeconds int       `json:"heartbeatIntervalSeconds"`
}

type ProbeHeartbeatRequest struct {
	Draining              bool     `json:"draining"`
	ActiveAssignmentIDs   []string `json:"activeAssignmentIds"`
	AvailableCapabilities []string `json:"availableCapabilities"`
	AvailableSlots        int      `json:"availableSlots"`
	Health                string   `json:"health"`
	ReasonCode            string   `json:"reasonCode,omitempty"`
}

type ProbeHeartbeatResponse struct {
	ServerTime             time.Time `json:"serverTime"`
	Drain                  bool      `json:"drain"`
	MinimumRuntimeVersion  string    `json:"minimumRuntimeVersion"`
	MinimumBrowserRevision string    `json:"minimumBrowserRevision"`
}

type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Theater struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerId"`
	SourceKey  string `json:"sourceKey"`
	Region     string `json:"region"`
	Name       string `json:"name"`
}

type AssignmentTask struct {
	Kind           string      `json:"kind"`
	Theater        Theater     `json:"theater"`
	Auditorium     *Auditorium `json:"auditorium,omitempty"`
	Showtime       *Showtime   `json:"showtime,omitempty"`
	TargetDates    []string    `json:"targetDates"`
	Locale         string      `json:"locale"`
	TimeZone       string      `json:"timeZone"`
	EgressPolicyID string      `json:"egressPolicyId"`
}

type ClaimAssignmentResponse struct {
	AssignmentID   string         `json:"assignmentId"`
	LeaseToken     string         `json:"leaseToken"`
	LeaseExpiresAt time.Time      `json:"leaseExpiresAt"`
	NotBefore      time.Time      `json:"notBefore"`
	Deadline       time.Time      `json:"deadline"`
	Task           AssignmentTask `json:"task"`
}

type AssignmentHeartbeatResponse struct {
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
}

type Movie struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerId"`
	SourceKey  string `json:"sourceKey"`
	Title      string `json:"title"`
	PosterURL  string `json:"posterUrl,omitempty"`
}

type Auditorium struct {
	ID             string   `json:"id"`
	TheaterID      string   `json:"theaterId"`
	SourceKey      string   `json:"sourceKey"`
	Name           string   `json:"name"`
	ScreenTypes    []string `json:"screenTypes"`
	Capacity       int      `json:"capacity"`
	SeatMapVersion string   `json:"seatMapVersion,omitempty"`
}

type Showtime struct {
	ID             string     `json:"id"`
	ProviderID     string     `json:"providerId"`
	SourceKey      string     `json:"sourceKey"`
	TheaterID      string     `json:"theaterId"`
	Movie          Movie      `json:"movie"`
	Auditorium     Auditorium `json:"auditorium"`
	StartsAt       time.Time  `json:"startsAt"`
	EndsAt         time.Time  `json:"endsAt"`
	AvailableSeats int        `json:"availableSeats"`
	Capacity       int        `json:"capacity"`
	SoldOut        bool       `json:"soldOut"`
}

type CatalogSnapshot struct {
	Provider    Provider     `json:"provider"`
	Theaters    []Theater    `json:"theaters"`
	Movies      []Movie      `json:"movies"`
	Auditoriums []Auditorium `json:"auditoriums"`
	Showtimes   []Showtime   `json:"showtimes"`
	ObservedAt  time.Time    `json:"observedAt"`
}

type SeatMapVersion struct {
	ID           string          `json:"id"`
	AuditoriumID string          `json:"auditoriumId"`
	LayoutHash   string          `json:"layoutHash"`
	Capacity     int             `json:"capacity"`
	Layout       json.RawMessage `json:"layout"`
	ObservedAt   time.Time       `json:"observedAt"`
}

type CatalogIndex struct {
	Generation  int64        `json:"generation"`
	Providers   []Provider   `json:"providers"`
	Theaters    []Theater    `json:"theaters"`
	Movies      []Movie      `json:"movies"`
	Auditoriums []Auditorium `json:"auditoriums"`
	Showtimes   []Showtime   `json:"showtimes"`
}

type Capture struct {
	TargetDate string     `json:"targetDate"`
	Complete   bool       `json:"complete"`
	ObservedAt time.Time  `json:"observedAt"`
	ErrorCode  string     `json:"errorCode,omitempty"`
	Showtimes  []Showtime `json:"showtimes"`
}

type AssignmentResult struct {
	RunID      string           `json:"runId"`
	Status     string           `json:"status"`
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt time.Time        `json:"finishedAt"`
	Captures   []Capture        `json:"captures"`
	Catalog    *CatalogSnapshot `json:"catalog,omitempty"`
	SeatMap    *SeatMapVersion  `json:"seatMap,omitempty"`
}

type ResultReceipt struct {
	AssignmentID string `json:"assignmentId"`
	RunID        string `json:"runId"`
	ContentHash  string `json:"contentHash"`
	Status       string `json:"status"`
}

type ClientUser struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type AuthExchangeRequest struct {
	UserID      string `json:"userId"`
	AccessToken string `json:"accessToken"`
}

type AuthExchangeResponse struct {
	AccessToken      string               `json:"accessToken"`
	ExpiresAt        time.Time            `json:"expiresAt"`
	RefreshToken     string               `json:"refreshToken"`
	RefreshExpiresAt time.Time            `json:"refreshExpiresAt"`
	User             ClientUser           `json:"user"`
	Launch           *ClientLaunchContext `json:"launch,omitempty"`
}

type AuthRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type ClientPINExchangeRequest struct {
	PIN            string `json:"pin"`
	InstallationID string `json:"installationId"`
	DeviceID       string `json:"deviceId"`
}

type ClientDevice struct {
	InstallationID string    `json:"installationId"`
	UserID         string    `json:"userId"`
	DeviceID       string    `json:"deviceId"`
	Platform       string    `json:"platform"`
	Arch           string    `json:"arch"`
	AppVersion     string    `json:"appVersion"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ClientResource struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Revision  int64           `json:"revision"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

type ClientEvent struct {
	Sequence   int64           `json:"sequence"`
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Resource   EventResource   `json:"resource"`
	Data       json.RawMessage `json:"data"`
}

const (
	EventStreamActionReady      = "ready"
	EventStreamActionHeartbeat  = "heartbeat"
	EventStreamActionFullResync = "full_resync"

	EventStreamResetRetentionGap  = "retention_gap"
	EventStreamResetInvalidCursor = "invalid_cursor"
)

// EventStreamControl is the typed control plane carried by the Client SSE
// stream. Cursor is the latest event included in Central's authoritative
// snapshot; a full_resync requires Client to reload bootstrap state before it
// resumes from that cursor.
type EventStreamControl struct {
	Protocol          int    `json:"protocol"`
	ReleaseGeneration int64  `json:"releaseGeneration"`
	Cursor            int64  `json:"cursor"`
	Action            string `json:"action"`
	Reason            string `json:"reason,omitempty"`
}

type EventResource struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type ClientBootstrap struct {
	User        ClientUser       `json:"user"`
	Protocol    int              `json:"protocol"`
	EventCursor int64            `json:"eventCursor"`
	Revisions   map[string]int64 `json:"revisions"`
	Features    map[string]bool  `json:"features"`
	Device      *ClientDevice    `json:"device,omitempty"`
}

type ReleaseArtifact struct {
	URL        string `json:"url"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable string `json:"executable"`
}

// ReleasePayloadSchemaVersion identifies the persisted release payload shape.
// It is independent from product versions and must change before an
// incompatible ReleaseEnvelope payload is stored.
const ReleasePayloadSchemaVersion = 2

// ReleaseEnvelope makes release publication and persisted registry payloads
// self-describing. Publish APIs wrap a ReleaseSet, while Central persists each
// immutable component release in its own envelope.
type ReleaseEnvelope[Payload any] struct {
	SchemaVersion int     `json:"schemaVersion"`
	Payload       Payload `json:"payload"`
}

type ClientRelease struct {
	Channel                  string            `json:"channel"`
	Platform                 string            `json:"platform"`
	Arch                     string            `json:"arch"`
	Version                  string            `json:"version"`
	MinimumLauncherVersion   string            `json:"minimumLauncherVersion"`
	MinimumBrowserRevision   string            `json:"minimumBrowserRevision"`
	PlaywrightVersion        string            `json:"playwrightVersion"`
	Protocol                 int               `json:"protocol"`
	Artifact                 ReleaseArtifact   `json:"artifact"`
	ProbeBootstrapPublicKeys map[string]string `json:"probeBootstrapPublicKeys"`
	PublishedAt              time.Time         `json:"publishedAt"`
}

type BrowserRelease struct {
	Channel                      string          `json:"channel"`
	Platform                     string          `json:"platform"`
	Arch                         string          `json:"arch"`
	Revision                     string          `json:"revision"`
	CompatiblePlaywrightVersions []string        `json:"compatiblePlaywrightVersions"`
	Artifact                     ReleaseArtifact `json:"artifact"`
	PublishedAt                  time.Time       `json:"publishedAt"`
}

type PlaywrightRelease struct {
	Channel     string          `json:"channel"`
	Platform    string          `json:"platform"`
	Arch        string          `json:"arch"`
	Version     string          `json:"version"`
	Artifact    ReleaseArtifact `json:"artifact"`
	PublishedAt time.Time       `json:"publishedAt"`
}

type RuntimeRelease struct {
	Client     ClientRelease     `json:"client"`
	Browser    BrowserRelease    `json:"browser"`
	Playwright PlaywrightRelease `json:"playwright"`
}

type LauncherRelease struct {
	Channel     string          `json:"channel"`
	Platform    string          `json:"platform"`
	Arch        string          `json:"arch"`
	Version     string          `json:"version"`
	Protocol    int             `json:"protocol"`
	Launcher    ReleaseArtifact `json:"launcher"`
	PublishedAt time.Time       `json:"publishedAt"`
}

// ReleaseSet publishes every supported platform artifact for one immutable
// component version as one activation transaction. It is carried inside a
// ReleaseEnvelope so the wire and persisted payload schema is explicit.
type ReleaseSet[Release any] struct {
	Releases []Release `json:"releases"`
}

// ProbeRelease records the independently deployed multi-architecture Probe
// container. It is inventory and rollout metadata, not a desktop component.
type ProbeRelease struct {
	Channel         string    `json:"channel"`
	Version         string    `json:"version"`
	Protocol        int       `json:"protocol"`
	BrowserRevision string    `json:"browserRevision"`
	Image           string    `json:"image"`
	ImageDigest     string    `json:"imageDigest"`
	PublishedAt     time.Time `json:"publishedAt"`
}

type LaunchTicketRequest struct {
	InstallationID           string `json:"installationId"`
	DeviceID                 string `json:"deviceId"`
	ReleaseGeneration        int64  `json:"releaseGeneration"`
	ClientVersion            string `json:"clientVersion"`
	ArtifactSHA256           string `json:"artifactSha256"`
	Protocol                 int    `json:"protocol"`
	BrowserRevision          string `json:"browserRevision"`
	BrowserArtifactSHA256    string `json:"browserArtifactSha256"`
	PlaywrightVersion        string `json:"playwrightVersion"`
	PlaywrightArtifactSHA256 string `json:"playwrightArtifactSha256"`
	Nonce                    string `json:"nonce"`
}

type LaunchTicketResponse struct {
	LaunchTicket string    `json:"launchTicket"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type ClientSessionExchangeRequest struct {
	LaunchTicket string `json:"launchTicket"`
	ClientNonce  string `json:"clientNonce"`
}

type ClientLaunchContext struct {
	InstallationID           string `json:"installationId"`
	DeviceID                 string `json:"deviceId"`
	ReleaseGeneration        int64  `json:"releaseGeneration"`
	ClientVersion            string `json:"clientVersion"`
	ArtifactSHA256           string `json:"artifactSha256"`
	Protocol                 int    `json:"protocol"`
	BrowserRevision          string `json:"browserRevision"`
	BrowserArtifactSHA256    string `json:"browserArtifactSha256"`
	PlaywrightVersion        string `json:"playwrightVersion"`
	PlaywrightArtifactSHA256 string `json:"playwrightArtifactSha256"`
}

// ClientLaunchEnvelope is the exact one-time Launcher-to-Client handoff. The
// ticket and every selected runtime component identity travel together so
// Client can reject a mixed or stale installation before session exchange.
type ClientLaunchEnvelope struct {
	LaunchTicket string `json:"launchTicket"`
	ClientLaunchContext
}

type ProbeBootstrapTicketRequest struct {
	InstallationID string   `json:"installationId"`
	DeviceID       string   `json:"deviceId"`
	Capabilities   []string `json:"capabilities"`
	MaxConcurrency int      `json:"maxConcurrency"`
	Runtime        Runtime  `json:"runtime"`
}

type ProbeBootstrapTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ProbeBootstrapClaims is the signed, short-lived handoff from Central to a
// Client-embedded Probe. It deliberately contains no reusable user credential.
type ProbeBootstrapClaims struct {
	Issuer         string   `json:"iss"`
	Audience       string   `json:"aud"`
	UserID         string   `json:"sub"`
	TicketID       string   `json:"jti"`
	IssuedAt       int64    `json:"iat"`
	NotBefore      int64    `json:"nbf"`
	ExpiresAt      int64    `json:"exp"`
	InstallationID string   `json:"installationId"`
	DeviceID       string   `json:"deviceId"`
	Kind           string   `json:"kind"`
	Capabilities   []string `json:"capabilities"`
	MaxConcurrency int      `json:"maxConcurrency"`
	Runtime        Runtime  `json:"runtime"`
}

type ExecutionCommand struct {
	ID             string           `json:"id"`
	MonitorID      string           `json:"monitorId"`
	InstallationID string           `json:"installationId"`
	Attempt        int              `json:"attempt"`
	Payload        ExecutionPayload `json:"payload"`
	LeaseToken     string           `json:"leaseToken"`
	LeaseExpiresAt time.Time        `json:"leaseExpiresAt"`
	CreatedAt      time.Time        `json:"createdAt"`
}

type ExecutionPayload struct {
	Showtime   Showtime  `json:"showtime"`
	ObservedAt time.Time `json:"observedAt"`
}

type ExecutionClaimRequest struct {
	InstallationID string `json:"installationId"`
}

type ExecutionHeartbeatRequest struct {
	LeaseToken string `json:"leaseToken"`
}

type ExecutionHeartbeatResponse struct {
	LeaseExpiresAt time.Time `json:"leaseExpiresAt"`
}

type ExecutionResultRequest struct {
	LeaseToken string `json:"leaseToken"`
	Status     string `json:"status"`
	ReasonCode string `json:"reasonCode,omitempty"`
}
