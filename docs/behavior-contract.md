# Probe live-seat behavior contract

Probe advertises `cgv.seat-map.capture` and
`cgv.seat-availability.capture` from container runtimes. Both capabilities
produce the same contract result: `AssignmentResult.completed.live_seat`.

`live_seat` is atomic. It contains one `seatmap.LiveSeatObservation` with a
normalized static layout and the exact-showtime availability snapshot. The two
snapshots share the auditorium, deterministic `layout_hash`, and capture
timestamp. Probe never returns a layout-only or availability-only result.

Provider catalog entities use typed CGV identities; provider display text is
not used as identity. A `SeatAvailabilityTask` requires an exact typed
showtime and re-resolves it from the current CGV schedule before the seat page
is opened. A `SeatMapTask` requires typed theater and auditorium identities and
may include an exact typed showtime; when it does not, Probe searches target
dates in order and selects the earliest currently bookable showtime for the
requested auditorium. It never substitutes a nearby auditorium or showtime.

The same provider response supplies both layout and status. Seat labels use an
uppercase row and canonical decimal seat number before the
auditorium-scoped Cineko seat ID is built; provider padding is not preserved.
Every provider seat must map to the static layout and carry a recognized sale
flag and status. Missing, duplicate, partial, or identity-mismatched data
fails closed; an empty available-seat set is valid only when the complete
response proves it.

If no requested date can be selected, Probe returns
`Deferred.reason.target_date_unavailable`. If selectable dates contain no
currently bookable matching showtime, it returns
`Deferred.reason.no_bookable_showtime`. Provider failures are returned through
the typed `collection.FailureReason` oneof (including identity mismatch,
blocked, throttled, CAPTCHA, authentication, UI drift, browser start,
transport, server, invalid result, and timeout cases). There is no legacy
result fallback, retryable boolean, reason-code field, or legacy provider
endpoint fallback.

Before returning a typed deferred or failed result, Probe records only the Go
error class in structured logs through `provider_error_summary`. It never reads
the provider-controlled error text for that attribute, so URLs, userinfo,
credentials, headers, tokens, and cookies cannot enter the summary.

## Central wire boundary

Probe uses only the latest generated service request and response messages for disconnect, assignment claim, lease heartbeat, and result submission. Claiming no work is a successful typed `ClaimAssignmentResponse.no_assignment`; an empty body or HTTP 204 is a contract failure. Result submission requires a generated response with a non-nil receipt. Unknown ProtoJSON fields and direct inner-message top-level bodies fail closed.

Probe release publication constructs `PublishProbeRequest` with generated Go code and strictly decodes `PublishProbeResponse`. Shell and `jq` may not author release-registry wire JSON. A successful publication requires a non-empty generated response and positive release-generation header.
