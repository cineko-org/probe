// Package egress owns Probe outbound-network policy and lease lifecycle.
//
// Direct proxies and the managed Soxy provider are implementation choices
// behind this capability. Callers select a purpose or assignment policy and
// receive a bounded lease without depending on provider transport details.
// The Client's embedded scanner is the Cineko consumer of managed Soxy
// inventory. A confirmed CGV HTTP 403 quarantines that lease before reuse;
// throttling and authentication failures do not request address rotation.
package egress
