// dreamtransctl runs host-local deployment and backup operations without Python.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dreamtrans/backend/internal/ops"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	if err := ops.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "[失败]", err)
		cancel()
		os.Exit(1)
	}
	cancel()
}
