// Command server is the hive storage reservation service.
//
// Usage:
//
//	server [serve] [-migrate] [-seed]       run the HTTP API (default command)
//	server migrate                          apply database migrations
//	server seed                             insert seed data (idempotent)
//	server account <id|username>            print account data (usage, active reservations) as JSON
//	server clear-reservations <id|username> permanently delete all reservations of an account
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/logging"
	"github.com/oboforty/hiveproj/internal/settings"
	"github.com/oboforty/hiveproj/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, logCloser, err := logging.New(cfg)
	if err != nil {
		return err
	}
	defer logCloser.Close()

	// Root context: cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	switch cmd {
	case "serve":
		return runServe(ctx, args, cfg, log, db)

	case "migrate":
		return database.Migrate(ctx, db, log)

	case "seed":
		return database.Seed(ctx, db, log, defaultMaxParallelChunks(ctx, cfg, log))

	case "account":
		if len(args) != 1 {
			return errors.New("usage: account <id|username>")
		}
		st := store.New(db)
		acc, err := st.FindAccount(ctx, args[0])
		if err != nil {
			return err
		}
		details, err := st.AccountDetails(ctx, acc)
		if err != nil {
			return err
		}
		return printJSON(os.Stdout, details)

	case "clear":
		if len(args) != 1 {
			return errors.New("usage: clear-reservations <id|username>")
		}
		st := store.New(db)
		acc, err := st.FindAccount(ctx, args[0])
		if err != nil {
			return err
		}
		n, err := st.ClearAccountReservations(ctx, acc.ID)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "cleared account reservations", "account_id", acc.ID, "deleted", n)
		return printJSON(os.Stdout, map[string]any{"account_id": acc.ID, "username": acc.Username, "deleted": n})

	default:
		return fmt.Errorf("unknown command %q (expected serve, migrate, seed, account, clear-reservations)", cmd)
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// defaultMaxParallelChunks resolves the max_parallel_chunks of new accounts
// from hivesettings.json, HIVE_API or ACCOUNT_MAX_CHUNKS_DEFAULT.
func defaultMaxParallelChunks(ctx context.Context, cfg config.Config, log *slog.Logger) int {
	return settings.GetDefaultMaxParallelChunks(ctx, settings.Sources{
		File:     cfg.SettingsFile,
		APIURL:   cfg.HiveAPI,
		Fallback: cfg.AccountMaxChunksDefault,
	}, log)
}
