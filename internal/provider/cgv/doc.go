// Package cgv owns CGV provider identity and browser-facing page behavior for
// Probe capture capabilities. Task scheduling and outbound-network policy
// belong to their owning packages.
//
// The source of truth for the provider boundary is
// contracts/docs/cgv-provider-contract.md in the sibling contracts
// repository. Provider parsing must fail closed rather than inventing an
// identity fallback when response evidence is unavailable.
package cgv
