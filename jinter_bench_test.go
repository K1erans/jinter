package jinter

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/K1erans/jinter/cache"
	"github.com/K1erans/jinter/jev"
	"github.com/K1erans/jinter/search"
)

// benchRepo writes a realistic multi-language git repository under dir.
func benchRepo(b *testing.B, dir string) {
	b.Helper()
	write := func(name string, body []byte) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	var goBuf strings.Builder
	goBuf.WriteString("package billing\n\nimport \"errors\"\n\n")
	for f := range 12 {
		fmt.Fprintf(&goBuf, "// Process%d handles step %d of a billing job.\nfunc Process%d(id string, attempt int) error {\n", f, f, f)
		for l := range 10 {
			fmt.Fprintf(&goBuf, "\tif attempt > %d { return errors.New(\"limit\") }\n", l+1)
		}
		goBuf.WriteString("\treturn nil\n}\n\n")
	}
	goFile := []byte(goBuf.String())
	goMatch := []byte(strings.Replace(goBuf.String(), "return nil", "return retryPayment(id)", 1))

	var tsBuf strings.Builder
	for f := range 8 {
		fmt.Fprintf(&tsBuf, "export function handleEvent%d(id: string, attempt: number): boolean {\n", f)
		for l := range 10 {
			fmt.Fprintf(&tsBuf, "  if (attempt > %d) return false;\n", l+1)
		}
		tsBuf.WriteString("  return true;\n}\n\n")
	}
	tsFile := []byte(tsBuf.String())
	bin := append([]byte("\x89PNG\x00\x00"), make([]byte, 4096)...)

	for pkg := range 25 {
		base := fmt.Sprintf("internal/pkg%02d", pkg)
		for i := range 6 {
			body := goFile
			if pkg == 12 && i == 3 {
				body = goMatch
			}
			write(fmt.Sprintf("%s/service_%02d.go", base, i), body)
		}
		write(base+"/service_test.go", goFile)
		write(base+"/README.md", []byte("# Package docs\n"))
		write(fmt.Sprintf("web/src/mod%02d/view.ts", pkg), tsFile)
		write(fmt.Sprintf("web/public/img%02d.png", pkg), bin)
		write(fmt.Sprintf("web/locales/en_%02d.json", pkg), []byte(`{"ok":"ok"}`))
	}
	write(".gitignore", []byte("/build\n*.log\nnode_modules/\n"))
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git init: %v\n%s", err, out)
	}
}

func dbDiskBytes(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

func BenchmarkSearchEndToEnd(b *testing.B) {
	repoDir := b.TempDir()
	benchRepo(b, repoDir)
	b.Chdir(repoDir)
	questions := []string{"retries failed payments", "sends receipt email", "validates input"}

	b.Run("warm-cached-sqlite", func(b *testing.B) {
		dbPath := filepath.Join(b.TempDir(), "cache.db")
		// Seed cache with a real fake client first.
		seedEngine := &Engine{
			Client: fakeClient{},
			OpenCache: func(_, model string) (cache.Cache, func() error, error) {
				c, err := cache.Open(dbPath, model)
				if err != nil {
					return nil, nil, err
				}
				return c, c.Close, nil
			},
		}
		if _, err := seedEngine.Search(context.Background(), Query{Questions: questions, Threshold: 0.6, Top: 10}); err != nil {
			b.Fatal(err)
		}
		disk := dbDiskBytes(dbPath)
		e := &Engine{
			OpenCache: seedEngine.OpenCache,
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			rep, err := e.Search(context.Background(), Query{Questions: questions, Threshold: 0.6, Top: 10})
			if err != nil || !rep.Matched() {
				b.Fatalf("err=%v matched=%v", err, rep.Matched())
			}
		}
		b.ReportMetric(float64(disk), "disk-B")
	})

	b.Run("cold-sqlite-fake", func(b *testing.B) {
		var totalDisk int64
		var totalTTFM int64
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			dbPath := filepath.Join(b.TempDir(), "cache.db")
			e := &Engine{
				Client: fakeClient{},
				OpenCache: func(_, model string) (cache.Cache, func() error, error) {
					c, err := cache.Open(dbPath, model)
					if err != nil {
						return nil, nil, err
					}
					return c, c.Close, nil
				},
			}
			var firstMatch atomic.Int64
			b.StartTimer()
			start := time.Now()
			rep, err := e.Search(context.Background(), Query{
				Questions: questions,
				Threshold: 0.6,
				Top:       10,
				OnMatch: func(_ string, _ search.Match) {
					firstMatch.CompareAndSwap(0, time.Since(start).Nanoseconds())
				},
			})
			b.StopTimer()
			if err != nil || !rep.Matched() {
				b.Fatalf("err=%v matched=%v", err, rep.Matched())
			}
			totalDisk += dbDiskBytes(dbPath)
			totalTTFM += firstMatch.Load()
			b.StartTimer()
		}
		b.ReportMetric(float64(totalDisk)/float64(b.N), "disk-B/op")
		b.ReportMetric(float64(totalTTFM)/float64(b.N)/1e6, "ttfm-ms")
	})

	b.Run("cold-http-wire", func(b *testing.B) {
		var reqBytes, respBytes, calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			reqBytes.Add(int64(len(body)))
			calls.Add(1)
			nq := strings.Count(string(body), `":{"type":"noul"`)
			hasMatch := strings.Contains(string(body), "retryPayment")
			var out strings.Builder
			out.Grow(64 + nq*32)
			out.WriteString(`{"model":"jev-1.13.0","answers":{`)
			for i := range nq {
				if i > 0 {
					out.WriteByte(',')
				}
				score := "0.05"
				if hasMatch && i%3 == 0 {
					score = "0.94"
				}
				fmt.Fprintf(&out, `"q%d":{"type":"noul","noul":%s}`, i, score)
			}
			out.WriteString(`}}`)
			resp := out.String()
			respBytes.Add(int64(len(resp)))
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, resp)
		}))
		srv.Config.ErrorLog = log.New(io.Discard, "", 0)
		defer srv.Close()

		client := jev.NewHTTP("bench-key", jev.HTTPOptions{
			BaseURL:           srv.URL,
			RequestsPerSecond: 1e9,
			TokensPerSecond:   1e12,
		})
		var totalDisk, totalTTFM int64
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			b.StopTimer()
			dbPath := filepath.Join(b.TempDir(), "cache.db")
			e := &Engine{
				Client: client,
				OpenCache: func(_, model string) (cache.Cache, func() error, error) {
					c, err := cache.Open(dbPath, model)
					if err != nil {
						return nil, nil, err
					}
					return c, c.Close, nil
				},
			}
			var firstMatch atomic.Int64
			b.StartTimer()
			start := time.Now()
			rep, err := e.Search(context.Background(), Query{
				Questions: questions,
				Threshold: 0.6,
				Top:       10,
				OnMatch: func(_ string, _ search.Match) {
					firstMatch.CompareAndSwap(0, time.Since(start).Nanoseconds())
				},
			})
			b.StopTimer()
			if err != nil || !rep.Matched() {
				b.Fatalf("err=%v matched=%v", err, rep.Matched())
			}
			totalDisk += dbDiskBytes(dbPath)
			totalTTFM += firstMatch.Load()
			b.StartTimer()
		}
		b.ReportMetric(float64(reqBytes.Load())/float64(b.N), "wire-req-B/op")
		b.ReportMetric(float64(respBytes.Load())/float64(b.N), "wire-resp-B/op")
		b.ReportMetric(float64(calls.Load())/float64(b.N), "http-calls/op")
		b.ReportMetric(float64(totalDisk)/float64(b.N), "disk-B/op")
		b.ReportMetric(float64(totalTTFM)/float64(b.N)/1e6, "ttfm-ms")
	})
}

type fakeClient struct{}

func (fakeClient) Ask(_ context.Context, req jev.Request) (jev.Response, error) {
	nouls := make([][]float64, len(req.Chunks))
	for i, ch := range req.Chunks {
		row := make([]float64, len(ch.Questions))
		hit := strings.Contains(ch.Code, "retryPayment")
		for j := range ch.Questions {
			if hit && j == 0 {
				row[j] = 0.94
			} else {
				row[j] = 0.05
			}
		}
		nouls[i] = row
	}
	return jev.Response{Model: req.Model, Nouls: nouls}, nil
}
