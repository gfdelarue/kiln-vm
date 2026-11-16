package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gfdelarue/kiln-vm/internal/config"
	"github.com/gfdelarue/kiln-vm/internal/sdnotify"
	"github.com/gfdelarue/kiln-vm/internal/server"
	"github.com/gfdelarue/kiln-vm/internal/store"
	"github.com/gfdelarue/kiln-vm/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("kiln-vmd version %s\n", version.Version)
		return 0
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	notifier := sdnotify.FromEnvironment()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid kiln-vm configuration", "error", err)
		return config.ExitCodeConfig
	}
	if len(os.Args) == 2 && (os.Args[1] == "--check-config" || os.Args[1] == "check-config") {
		return 0
	}

	listeners, err := server.BindListeners(cfg)
	if err != nil {
		slog.Error("failed to bind kiln-vm management transport", "error", err)
		if errors.Is(err, server.ErrInvalidManagementTransport) {
			return config.ExitCodeConfig
		}
		return 1
	}
	defer listeners.Close()

	db, err := store.Open(cfg.DataDir)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer db.Close()

	srv, err := server.New(cfg, db, listeners)
	if err != nil {
		slog.Error("failed to initialize kiln-vm server", "error", err)
		return 1
	}
	// Preserve the operating system's default immediate termination while
	// synchronous provider recovery is running. Once initialization completes,
	// take ownership of graceful shutdown before advertising readiness.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Run server in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.StartWithReady(func() error {
			select {
			case sig := <-sigCh:
				return fmt.Errorf("startup interrupted by signal %s", sig)
			default:
			}
			return notifier.Ready("kiln-vm management transport ready")
		})
	}()

	// Wait for signal or server error.
	serverFailed := false
	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		if err != nil {
			slog.Error("server failed", "error", err)
			serverFailed = true
		}
	}

	// Graceful shutdown with 30s timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
		return 1
	}
	slog.Info("shutdown complete")
	if serverFailed {
		return 1
	}
	return 0
}
