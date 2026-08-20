// Package browser owns the Probe's browser-task lifecycle.
//
// It allocates isolated task slots, applies task-specific browser identity,
// and releases browser resources when the task ends. Provider behavior remains
// in the provider adapter packages; this package only composes that behavior
// with process and lease lifecycle rules.
package browser
