package protocol

import (
	"errors"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

const ProtocolVersion = contracts.ProtocolVersion

type Runtime = contracts.Runtime
type RegisterProbeRequest = contracts.RegisterProbeRequest
type RegisterProbeResponse = contracts.RegisterProbeResponse
type ProbeHeartbeatRequest = contracts.ProbeHeartbeatRequest
type ProbeHeartbeatResponse = contracts.ProbeHeartbeatResponse
type Theater = contracts.Theater
type AssignmentTask = contracts.AssignmentTask
type ClaimAssignmentResponse = contracts.ClaimAssignmentResponse
type AssignmentHeartbeatResponse = contracts.AssignmentHeartbeatResponse
type Movie = contracts.Movie
type Auditorium = contracts.Auditorium
type Showtime = contracts.Showtime
type Capture = contracts.Capture
type AssignmentResult = contracts.AssignmentResult
type ResultReceipt = contracts.ResultReceipt

var ErrUnauthorized = errors.New("unauthorized")

// RegistrationAuthorization is local Central-facing authorization state. It
// is never serialized and therefore deliberately remains outside Contracts.
type RegistrationAuthorization struct {
	OwnerUserID string
	DeviceID    string
	TicketID    string
	ExpiresAt   time.Time
}
