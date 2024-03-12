package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/pprof/profile"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"github.com/sourcegraph/conc/pool"
)

const (
	name    = "datadog-pgo"
	version = "0.0.1"
)

// main runs the pgo tool.
func main() {
	if err := run(); err != nil && !errors.As(err, &handledError{}) {
		if !errors.As(err, &loggedError{}) {
			fmt.Fprintf(os.Stderr, "pgo: error: %v\n", err)
		}
		os.Exit(1)
	}
}

// run runs the pgo tool and returns an error if any.
func run() (err error) {
	start := time.Now()

	// Define usage
	flag.Usage = func() {
		usage := `usage: ` + name + ` [OPTIONS]... QUERY... DEST

` + name + ` fetches CPU profiles from Datadog using the given QUERY arguments
and merges the results into a single DEST file suitable for profile-guided
optimization.

In order to use this, you need to set the following environment variables.

	DD_API_KEY: A Datadog API key
	DD_APP_KEY: A Datadog Application key
	DD_SITE: A Datadog site to use (defaults to datadoghq.com)

After this, typical usage will look like this:

	` + name + ` 'service:my-service env:prod' ./cmd/my-service/default.pgo

The go toolchain will automatically pick up any default.pgo file found in the
main package (go1.21+), so you can build your service as usual, for example:

	go build ./cmd/my-service

Unless the -fail flag is set, datadog-pgo will always return with a zero exit
code in order to let your build succeed, even if no pgo downloading failed.

OPTIONS`
		fmt.Fprintln(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}

	// Parse flags
	var (
		failF     = flag.Bool("fail", false, "return with a non-zero exit code on failure")
		jsonF     = flag.Bool("json", false, "print logs in json format")
		profilesF = flag.Int("profiles", 5, "the number of profiles to fetch per query")
		timeoutF  = flag.Duration("timeout", 60*time.Second, "timeout for fetching pgo profile")
		verboseF  = flag.Bool("v", false, "verbose output")
		windowF   = flag.Duration("window", 3*24*time.Hour, "how far back to search for profiles")
	)
	flag.Parse()

	// Validate args
	if flag.NArg() < 2 {
		flag.Usage()
		return errors.New("at least 2 arguments are required")
	}

	// Split args into queries and dst
	queries := buildQueries(*windowF, flag.Args()[:flag.NArg()-1])
	dst := flag.Arg(flag.NArg() - 1)

	// Setup logger
	logOpt := &slog.HandlerOptions{AddSource: *verboseF}
	if *verboseF {
		logOpt.Level = slog.LevelDebug
	}
	log := slog.New(tint.NewHandler(os.Stdout, &tint.Options{
		AddSource:  logOpt.AddSource,
		Level:      logOpt.Level,
		TimeFormat: "",
		NoColor:    !isatty.IsTerminal(os.Stdout.Fd()),
	}))
	if *jsonF {
		log = slog.New(slog.NewJSONHandler(os.Stdout, logOpt))
	}
	log.Info(name, "version", version, "go-version", runtime.Version())

	// Log errors and turn them into warnings unless -fail is set
	defer func() {
		if err == nil {
			return
		}
		log.Error(err.Error())
		err = loggedError{err}
		if !*failF {
			err = handledError{err}
			log.Warn(name + " failed, but -fail is not set, returning exit code 0 to continue without pgo")
		}
	}()

	// Setup API client
	client, err := ClientFromEnv()
	if err != nil {
		return fmt.Errorf("clientFromEnv: %w", err)
	}

	// Create context
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutF)
	defer cancel()

	// Search, download and merge profiles
	mergedProfile, err := SearchDownloadMerge(ctx, log, *profilesF, client, queries)
	if err != nil {
		return err
	}

	// Writing pgo file to dst
	n, err := mergedProfile.Write(dst)
	if err != nil {
		return err
	}
	log.Info(
		"wrote pgo file",
		"path", dst,
		"samples", mergedProfile.Samples(),
		"bytes", n,
		"total-duration", timeSinceRoundMS(start),
	)
	return nil
}

// buildQueries returns a list of SearchQuery for the given time window and queries.
func buildQueries(window time.Duration, queries []string) (searchQueries []SearchQuery) {
	searchQueries = make([]SearchQuery, 0, len(queries))
	for _, q := range queries {
		// PGO is only supported for Go right now, avoid fetching non-go
		// profiles (e.g. from native) that might exist for the same query.
		if !strings.Contains(q, "language:go") && !strings.Contains(q, "runtime:go") {
			q = strings.TrimSpace(q) + " runtime:go"
		}

		searchQueries = append(searchQueries, SearchQuery{
			Filter: SearchFilter{
				From:  JSONTime{time.Now().Add(-window)},
				To:    JSONTime{time.Now()},
				Query: q,
			},
			Sort: SearchSort{
				Order: "desc",
				// TODO(fg) or use @metrics.core_cpu_time_total?
				Field: "@metrics.core_cpu_cores",
			},
		})
	}
	return
}

// SearchDownloadMerge queries the profiles, downloads them and merges them into a single profile.
func SearchDownloadMerge(ctx context.Context, log *slog.Logger, numProfiles int, client *Client, queries []SearchQuery) (*MergedProfile, error) {
	newPool := func() *pool.ContextPool {
		return pool.New().WithErrors().WithContext(ctx).WithCancelOnError().WithFirstError()
	}

	var pgoProfile = &MergedProfile{}
	queryPool := newPool()
	downloadPool := newPool()
	for _, q := range queries {
		q := q
		queryPool.Go(func(ctx context.Context) error {
			log.Info(
				"searching profiles",
				"query", q.Filter.Query,
				"by", q.Sort.Field,
				"order", q.Sort.Order,
				"from", q.Filter.From.String(),
				"to", q.Filter.To.String(),
			)
			startQuery := time.Now()
			profiles, err := client.SearchProfiles(ctx, q)
			if err != nil {
				return err
			}
			log.Debug(
				"found profiles",
				"count", len(profiles),
				"duration", timeSinceRoundMS(startQuery),
				"query", q.Filter.Query,
			)

			if len(profiles) > numProfiles {
				profiles = profiles[:numProfiles]
			}

			for _, p := range profiles {
				p := p
				downloadPool.Go(func(ctx context.Context) error {
					log.Info(
						"downloading profile",
						"service", p.Service,
						"cpu-cores", float64(int(p.CPUCores*10))/10,
						"duration", p.Duration,
						"age", time.Since(p.Timestamp).Round(time.Second),
						"profile-id", p.ProfileID,
					)
					startDownload := time.Now()
					download, err := client.DownloadProfile(ctx, p)
					if err != nil {
						return err
					}
					log.Debug(
						"downloaded profile",
						"duration", timeSinceRoundMS(startDownload),
						"bytes", len(download.data),
						"profile-id", p.ProfileID,
						"event-id", p.EventID,
					)

					cpu, err := download.ExtractCPUProfile()
					if err != nil {
						return err
					}

					prof, err := profile.ParseData(cpu)
					if err != nil {
						return err
					}
					return pgoProfile.Merge(prof)
				})
			}
			return nil
		})
	}
	if err := queryPool.Wait(); err != nil {
		return nil, err
	} else if err := downloadPool.Wait(); err != nil {
		return nil, err
	}
	return pgoProfile, nil
}

// MergedProfile is the result of merging multiple profiles.
type MergedProfile struct {
	mu      sync.Mutex
	profile *profile.Profile
}

// Merge merges prof into the current profile. Callers must not use prof after
// calling Merge.
func (p *MergedProfile) Merge(prof *profile.Profile) (err error) {
	// Drop labels to reduce profile size
	for _, s := range prof.Sample {
		s.Label = nil
	}

	// Acquire lock to access p.profile.
	p.mu.Lock()
	defer p.mu.Unlock()

	// First profile? No need to merge.
	if p.profile == nil {
		p.profile = prof
		return nil
	}

	// Merge profiles after the first one.
	p.profile, err = profile.Merge([]*profile.Profile{p.profile, prof})
	return
}

// Write writes the merged profile to dst and returns the number of bytes
// written.
func (p *MergedProfile) Write(dst string) (int64, error) {
	file, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	cw := &countingWriter{W: file}
	if err := p.profile.Write(cw); err != nil {
		return cw.N, err
	}
	return cw.N, file.Close()
}

// Samples returns the number of samples in the merged profile.
func (p *MergedProfile) Samples() int {
	return len(p.profile.Sample)
}

// ProfileDownload is the result of downloading a profile.
type ProfileDownload struct {
	data []byte
}

// ExtractCPUProfile extracts the CPU profile from the download.
func (d ProfileDownload) ExtractCPUProfile() ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(d.data), int64(len(d.data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) == "cpu.pprof" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}

	return nil, errors.New("no cpu.pprof found in download")
}

// wrapErr wraps the error with name if it is not nil.
func wrapErr(err *error, name string) {
	if *err != nil {
		*err = fmt.Errorf("%s: %w", name, *err)
	}
}

// timeSinceRoundMS returns the time since t rounded to the nearest millisecond.
func timeSinceRoundMS(t time.Time) time.Duration {
	return time.Since(t) / time.Millisecond * time.Millisecond
}

// countingWriter counts the number of bytes written to W.
type countingWriter struct {
	W io.Writer
	N int64
}

// Write writes p to W and updates N.
func (c *countingWriter) Write(p []byte) (n int, err error) {
	n, err = c.W.Write(p)
	c.N += int64(n)
	return
}

// loggedError is an error that has been logged.
type loggedError struct {
	error
}

// handledError is an error that has been handled.
type handledError struct {
	error
}
