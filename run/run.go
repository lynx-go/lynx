package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

type RunFunc func(ctx context.Context) error

func WaitForSignals(sigs ...os.Signal) RunFunc {
	return func(ctx context.Context) error {
		if len(sigs) == 0 {
			sigs = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
		}
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, sigs...)
		select {
		case <-quit:
			return nil
		case <-ctx.Done():
			err := ctx.Err()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}
