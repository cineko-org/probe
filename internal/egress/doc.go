// Package egress owns Probe outbound-network policy and lease lifecycle.
//
// Direct proxies and the managed Soxy provider are implementation choices
// behind this capability. Callers select a purpose or assignment policy and
// receive a bounded lease without depending on provider transport details.
package egress
