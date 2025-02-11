package run

import (
	"os"
	"os/signal"
	"syscall"
)

func ListenSignal() {
	// Handle graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	}
}
