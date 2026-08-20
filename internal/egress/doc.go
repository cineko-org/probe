// Package egress owns outbound-network policy and lease lifecycle for Probe.
//
// Direct proxies and the managed Soxy provider are implementation choices
// behind this capability. Callers select a purpose or assignment policy and
// receive a bounded lease without depending on provider-specific transport
// details.
package egress
