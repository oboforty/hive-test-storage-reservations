package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func api(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestFromFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(file, []byte(`{"default_max_parallel_chunks": 7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, calls := api(t, http.StatusOK, `{"ENV": "dev", "MAX_PARALLEL_CHUNKS": 9}`)

	got := GetDefaultMaxParallelChunks(context.Background(), Sources{File: file, APIURL: srv.URL, Fallback: 5}, nil)
	if got != 7 {
		t.Errorf("got %d, want 7 (from file)", got)
	}
	if calls.Load() != 0 {
		t.Error("API must not be called when the file is valid")
	}
}

func TestFromAPIAndCached(t *testing.T) {
	file := filepath.Join(t.TempDir(), DefaultFile)
	srv, calls := api(t, http.StatusOK, `{"ENV": "dev", "MAX_PARALLEL_CHUNKS": 9}`)
	src := Sources{File: file, APIURL: srv.URL, Fallback: 5}

	if got := GetDefaultMaxParallelChunks(context.Background(), src, nil); got != 9 {
		t.Errorf("got %d, want 9 (from API)", got)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("settings file not cached: %v", err)
	}
	if !strings.Contains(string(data), `"default_max_parallel_chunks": 9`) {
		t.Errorf("unexpected cache content: %s", data)
	}

	// Second call is served from the cache file.
	if got := GetDefaultMaxParallelChunks(context.Background(), src, nil); got != 9 {
		t.Errorf("got %d, want 9 (from cache)", got)
	}
	if calls.Load() != 1 {
		t.Errorf("API called %d times, want 1", calls.Load())
	}
}

func TestInvalidFileFallsThroughToAPI(t *testing.T) {
	file := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(file, []byte(`{"default_max_parallel_chunks": 0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := api(t, http.StatusOK, `{"ENV": "dev", "MAX_PARALLEL_CHUNKS": 3}`)
	if got := GetDefaultMaxParallelChunks(context.Background(), Sources{File: file, APIURL: srv.URL, Fallback: 5}, nil); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestAPIFailuresUseFallback(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"404":          {http.StatusNotFound, `{"MAX_PARALLEL_CHUNKS": 9}`},
		"500":          {http.StatusInternalServerError, ``},
		"invalid json": {http.StatusOK, `nope`},
		"zero value":   {http.StatusOK, `{"MAX_PARALLEL_CHUNKS": 0}`},
		"missing key":  {http.StatusOK, `{}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), DefaultFile)
			srv, _ := api(t, tc.status, tc.body)
			if got := GetDefaultMaxParallelChunks(context.Background(), Sources{File: file, APIURL: srv.URL, Fallback: 5}, nil); got != 5 {
				t.Errorf("got %d, want fallback 5", got)
			}
			if _, err := os.Stat(file); err == nil {
				t.Error("failed API response must not be cached")
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv, _ := api(t, http.StatusOK, `{"ENV": "dev", "MAX_PARALLEL_CHUNKS": 9}`)
		url := srv.URL
		srv.Close()
		src := Sources{File: filepath.Join(t.TempDir(), DefaultFile), APIURL: url, Fallback: 5}
		if got := GetDefaultMaxParallelChunks(context.Background(), src, nil); got != 5 {
			t.Errorf("got %d, want fallback 5", got)
		}
	})

	t.Run("no API configured", func(t *testing.T) {
		src := Sources{File: filepath.Join(t.TempDir(), DefaultFile), Fallback: 5}
		if got := GetDefaultMaxParallelChunks(context.Background(), src, nil); got != 5 {
			t.Errorf("got %d, want fallback 5", got)
		}
	})
}
