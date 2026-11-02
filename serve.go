package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/oboforty/hiveproj/internal/api"
	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/store"
)

// runServe implements the "serve" command: parses its flags, optionally
// migrates/seeds the database, then runs the HTTP server.
func runServe(ctx context.Context, args []string, cfg config.Config, log *slog.Logger, db *sql.DB) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	migrate := fs.Bool("migrate", false, "apply migrations before starting")
	seed := fs.Bool("seed", false, "seed the database before starting (implies -migrate)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *migrate || *seed {
		if err := database.Migrate(ctx, db, log); err != nil {
			return err
		}
	}
	if *seed {
		if err := database.Seed(ctx, db, log, defaultMaxParallelChunks(ctx, cfg, log)); err != nil {
			return err
		}
	}
	return serve(ctx, cfg, log, db)
}

// serve runs the HTTP server. Startup and shutdown each run in their own
// goroutine; serve returns once the server has fully stopped.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger, db *sql.DB) error {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(store.New(db), dedup.NewStore(db), log, cfg).Router(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	serverErr := make(chan error, 1)
	shutdownDone := make(chan error, 1)

	// Startup handler.
	go func() {
		ln, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			serverErr <- fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
			return
		}
		log.Info("server started", "addr", ln.Addr().String(), "env", cfg.Env, "db", cfg.DBPath)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// Shutdown handler: waits for a signal (ctx cancelled) or a startup
	// failure, then drains in-flight requests within ShutdownTimeout.
	startupFailed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received, draining connections", "timeout", cfg.ShutdownTimeout)
		case <-startupFailed:
			shutdownDone <- nil
			return
		}
		shCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			log.Error("graceful shutdown failed, forcing close", "err", err)
			_ = srv.Close()
			shutdownDone <- err
			return
		}
		shutdownDone <- nil
	}()

	if err := <-serverErr; err != nil {
		close(startupFailed)
		<-shutdownDone
		log.Error("server error", "err", err)
		return err
	}
	if err := <-shutdownDone; err != nil {
		return err
	}
	log.Info("server stopped")
	return nil
}
