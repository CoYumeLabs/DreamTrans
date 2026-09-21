// Package deployment exposes the common admission and cooperative drain protocol
// to companion services in this repository.
package deployment

import (
	internal "github.com/dreamtrans/backend/internal/deployment"
	"io"
)

type Runtime = internal.Runtime
type Snapshot = internal.Snapshot

func Configure() error                              { return internal.Configure() }
func Default() *Runtime                             { return internal.Default }
func Control(action string, output io.Writer) error { return internal.Control(action, output) }
