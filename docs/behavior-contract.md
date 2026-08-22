# Probe live-seat behavior contract

Probe exposes `cgv.seat-availability.capture` only from container runtimes.
Each assignment names one canonical CGV theater, auditorium, and showtime. The
showtime is resolved again from the provider schedule on its schedule date;
Probe never chooses a nearby or first available showtime.

The seat response is parsed into two separate values:

- static layout geometry and metadata, hashed deterministically as
  `layout_hash`;
- a timestamped set of normalized Cineko seat IDs whose provider status is
  `seatSaleYn=Y` and `seatStusCd=00`.

Seat labels use an uppercase row and canonical decimal seat number before the
auditorium-scoped Cineko seat ID is built; provider padding is not preserved.

Every provider seat must map to the static layout and carry a recognized sale
flag and status. Missing, duplicate, partial, or identity-mismatched data
fails closed; an empty available-seat set is valid only when the complete
response proves it. `observed_at` is the time the live response was captured.

Authentication, CAPTCHA, UI-contract drift, throttling, and blocked egress are
reported as distinct failure reasons. A protection signal is never converted
to a successful empty-seat observation. Blocked egress, throttling, and capture
timeouts set `Failed.retryable=true`; authentication, CAPTCHA, UI drift, and
incomplete seat status remain terminal for that assignment.
