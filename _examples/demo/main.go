package main

import (
	"context"
	"github.com/lynx-go/lynx"
	"log/slog"
)

func main() {
	app := lynx.New(
		lynx.WithName("lynx-demo"),
		lynx.WithServices(&mockService{}),
		lynx.WithOnStart(func(ctx context.Context) error {
			slog.Info("start hook")
			return nil
		}),
		lynx.WithOnStop(func(ctx context.Context) error {
			slog.Info("stop hook")
			return nil
		}),
	)
	app.Run()
}

type mockService struct {
}

func (m *mockService) Name() string {
	return "mock"
}

func (m *mockService) Start(ctx context.Context) error {
	slog.Info("start mock service")
	return nil
}

func (m *mockService) Stop(ctx context.Context) error {
	slog.Info("stop mock service")
	return nil
}

var _ lynx.Service = new(mockService)
