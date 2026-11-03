// Package settings resolves service-wide defaults from (in order) a local
// hivesettings.json file, the remote HIVE_API, and finally an env fallback.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultFile is the default path of the local settings file.
const DefaultFile = "hivesettings.json"

// Settings is the content of hivesettings.json.
type Settings struct {
	DefaultMaxParallelChunks int `json:"default_max_parallel_chunks"`
}

// apiResponse is the subset of the HIVE_API JSON response we use. The API
// returns many other (ignored) keys.
type apiResponse struct {
	MaxParallelChunks int `json:"MAX_PARALLEL_CHUNKS"`
}

// Sources configures where GetDefaultMaxParallelChunks looks.
type Sources struct {
	// File is the local settings file (also used as cache for APIURL).
	File string
	// APIURL is fetched with GET when File is missing or invalid. Optional.
	APIURL string
	// Fallback is used when neither File nor APIURL yields a valid value
	// (ACCOUNT_MAX_CHUNKS_DEFAULT).
	Fallback int
	// Client performs the API request. Defaults to a client with a 5s timeout.
	Client *http.Client
}

// GetDefaultMaxParallelChunks returns the default max_parallel_chunks for new
// accounts:
//
//  1. read src.File (hivesettings.json);
//  2. if it is missing or invalid, GET src.APIURL and cache the result into
//     src.File;
//  3. if the API is not configured or fails (network error, non-2xx such as
//     404, bad JSON, value <= 0), use src.Fallback.
//
// Each failed step is logged; the function itself never fails.
func GetDefaultMaxParallelChunks(ctx context.Context, src Sources, log *slog.Logger) int {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	s, err := readFile(src.File)
	if err == nil {
		log.DebugContext(ctx, "default max_parallel_chunks from settings file",
			"file", src.File, "value", s.DefaultMaxParallelChunks)
		return s.DefaultMaxParallelChunks
	}
	if !errors.Is(err, fs.ErrNotExist) {
		log.WarnContext(ctx, "ignoring invalid settings file", "file", src.File, "err", err)
	}

	if src.APIURL != "" {
		s, err := fetch(ctx, src.Client, src.APIURL)
		if err == nil {
			if err := writeFile(src.File, s); err != nil {
				log.WarnContext(ctx, "could not cache settings", "file", src.File, "err", err)
			}
			log.InfoContext(ctx, "default max_parallel_chunks from HIVE_API",
				"url", src.APIURL, "value", s.DefaultMaxParallelChunks, "cached_to", src.File)
			return s.DefaultMaxParallelChunks
		}
		log.WarnContext(ctx, "HIVE_API request failed, using ACCOUNT_MAX_CHUNKS_DEFAULT",
			"url", src.APIURL, "err", err, "fallback", src.Fallback)
	}

	log.InfoContext(ctx, "default max_parallel_chunks from ACCOUNT_MAX_CHUNKS_DEFAULT", "value", src.Fallback)
	return src.Fallback
}

func (s Settings) validate() error {
	if s.DefaultMaxParallelChunks <= 0 {
		return fmt.Errorf("default_max_parallel_chunks must be > 0, got %d", s.DefaultMaxParallelChunks)
	}
	return nil
}

func decode(r io.Reader) (Settings, error) {
	var s Settings
	if err := json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("decode settings: %w", err)
	}
	return s, s.validate()
}

func decodeAPI(r io.Reader) (Settings, error) {
	var a apiResponse
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&a); err != nil {
		return Settings{}, fmt.Errorf("decode HIVE_API response: %w", err)
	}
	if a.MaxParallelChunks <= 0 {
		return Settings{}, fmt.Errorf("HIVE_API MAX_PARALLEL_CHUNKS must be > 0, got %d", a.MaxParallelChunks)
	}
	return Settings{DefaultMaxParallelChunks: a.MaxParallelChunks}, nil
}

func readFile(path string) (Settings, error) {
	f, err := os.Open(path)
	if err != nil {
		return Settings{}, err
	}
	defer f.Close()
	return decode(f)
}

func fetch(ctx context.Context, client *http.Client, url string) (Settings, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Settings{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Settings{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Settings{}, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return decodeAPI(resp.Body)
}

// writeFile writes atomically (temp file + rename) so a crash never leaves a
// half-written settings file behind.
func writeFile(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hivesettings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
