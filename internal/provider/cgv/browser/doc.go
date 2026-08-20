// Package browser owns the CGV browser-task lifecycle used by Probe.
//
// It allocates isolated task slots, applies task-specific browser identity,
// and releases browser resources when a task ends. CGV page behavior remains
// in the parent provider package; this package composes it with process and
// egress lease lifecycle rules.
package browser
