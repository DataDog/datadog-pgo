// Package pgo contains the profile fetching and merging logic used by the
// datadog-pgo command-line tool. It is an internal implementation detail of
// the tool and not a supported public API; consumers should invoke the
// datadog-pgo command directly.
package pgo

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/sourcegraph/conc/pool"
)

// Name and Version identify this library in the User-Agent header sent to the
// Datadog API and are exposed so the CLI can log them.
const (
	Name    = "datadog-pgo"
	Version = "0.0.1"
)

// Default values for Options. These mirror the CLI's flag defaults.
const (
	DefaultProfilesPerQuery = 5
	DefaultWindow           = 3 * 24 * time.Hour
)

// Options configures a Fetch call. All fields are optional; the zero value
// works as long as DD_API_KEY and DD_APP_KEY are set in the environment.
type Options struct {
	// APIKey, AppKey, Site override the corresponding DD_API_KEY,
	// DD_APP_KEY and DD_SITE environment variables. If empty, the env
	// var is used. Site defaults to "datadoghq.com".
	APIKey, AppKey, Site string

	// Logger receives progress logs. If nil, logs are discarded.
	Logger *slog.Logger

	// Timeout bounds the entire Fetch call. If zero, the context's
	// deadline (if any) is used unmodified.
	Timeout time.Duration

	// ProfilesPerQuery is the number of profiles to fetch for each query.
	// If <= 0, defaults to DefaultProfilesPerQuery.
	ProfilesPerQuery int

	// Window is how far back to search for profiles. If <= 0, defaults
	// to DefaultWindow.
	Window time.Duration

	// SoftFail makes Fetch log errors via Logger and return nil instead
	// of returning them. Use this in build pipelines that should keep
	// going when PGO data is unavailable.
	SoftFail bool
}

// Fetch is the high-level entry point. It builds search queries from the given
// query strings, downloads matching profiles from Datadog, merges them with
// the no-inline hack applied, and writes the result to dst as a .pgo file
// suitable for `go build -pgo=<dst>`.
//
// Fetch is equivalent to the datadog-pgo CLI's default behavior. For finer
// control, use BuildQueries + SearchDownloadMerge + MergedProfile directly.
func Fetch(ctx context.Context, queries []string, dst string, opts Options) (err error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.ProfilesPerQuery <= 0 {
		opts.ProfilesPerQuery = DefaultProfilesPerQuery
	}
	if opts.Window <= 0 {
		opts.Window = DefaultWindow
	}

	defer func() {
		if err == nil || !opts.SoftFail {
			return
		}
		log.Warn(Name+" failed, continuing without PGO", "err", err)
		err = nil
	}()

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	client, err := newClientFromOptions(opts)
	if err != nil {
		return err
	}

	searchQueries := BuildQueries(opts.Window, opts.ProfilesPerQuery, queries)
	merged, err := SearchDownloadMerge(ctx, log, client, searchQueries)
	if err != nil {
		return err
	}
	if err := merged.ApplyNoInlineHack(); err != nil {
		return err
	}
	n, err := merged.Write(dst)
	if err != nil {
		return err
	}
	log.Info(
		"wrote PGO file",
		"path", dst,
		"samples", merged.Samples(),
		"bytes", n,
		"debug-query", merged.DebugQuery(),
	)
	return nil
}

// newClientFromOptions builds a Client using opts, falling back to environment
// variables for any unset credential.
func newClientFromOptions(opts Options) (*Client, error) {
	api := opts.APIKey
	if api == "" {
		api = os.Getenv("DD_API_KEY")
	}
	app := opts.AppKey
	if app == "" {
		app = os.Getenv("DD_APP_KEY")
	}
	site := opts.Site
	if site == "" {
		site = os.Getenv("DD_SITE")
	}
	if site == "" {
		site = "datadoghq.com"
	}
	if api == "" {
		return nil, errors.New("DD_API_KEY (or Options.APIKey) is required")
	}
	if app == "" {
		return nil, errors.New("DD_APP_KEY (or Options.AppKey) is required")
	}
	return &Client{
		site:        site,
		apiKey:      api,
		appKey:      app,
		concurrency: make(chan struct{}, maxConcurrency),
	}, nil
}

// BuildQueries returns a list of SearchQuery for the given time window and queries.
// Each query is automatically augmented with `runtime:go` when neither
// `runtime:go` nor `language:go` is already present, so that non-Go profiles
// matching the same filter are not fetched.
func BuildQueries(window time.Duration, limit int, queries []string) []SearchQuery {
	searchQueries := make([]SearchQuery, 0, len(queries))
	for _, q := range queries {
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
			Limit: limit,
		})
	}
	return searchQueries
}

// usePGOEndpoint is a flag to use the pgo endpoint instead of the search and
// download endpoints. If this new endpoint proves to work well, we can remove
// this flag and the old code.
const usePGOEndpoint = true

// SearchDownloadMerge queries the profiles, downloads them and merges them into a single profile.
func SearchDownloadMerge(ctx context.Context, log *slog.Logger, client *Client, queries []SearchQuery) (*MergedProfile, error) {
	if usePGOEndpoint {
		return searchDownloadMergePGOEndpoint(ctx, log, client, queries)
	}
	return searchDownloadMerge(ctx, log, client, queries)
}

// searchDownloadMerge queries the profiles, downloads them and merges them into a single profile.
func searchDownloadMerge(ctx context.Context, log *slog.Logger, client *Client, queries []SearchQuery) (*MergedProfile, error) {
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

			if len(profiles) > q.Limit {
				profiles = profiles[:q.Limit]
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
					return pgoProfile.Merge(p.ProfileID, prof)
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

// searchDownloadMergePGOEndpoint queries the profiles and downloads them using
// the new pgo endpoint. Then it merges hte profiles into a single profile using
// the pgo endpoint.
func searchDownloadMergePGOEndpoint(ctx context.Context, log *slog.Logger, client *Client, queries []SearchQuery) (*MergedProfile, error) {
	download, err := client.SearchAndDownloadProfiles(ctx, queries)
	if err != nil {
		return nil, err
	}
	return download.MergedProfile(log)
}

// MergedProfile is the result of merging multiple profiles.
type MergedProfile struct {
	mu         sync.Mutex
	profile    *profile.Profile
	profileIDs []string
}

// Merge merges prof into the current profile. Callers must not use prof after
// calling Merge.
func (p *MergedProfile) Merge(id string, prof *profile.Profile) (err error) {
	// Drop labels to reduce profile size
	for _, s := range prof.Sample {
		s.Label = nil
	}

	// Acquire lock to access p fields
	p.mu.Lock()
	defer p.mu.Unlock()

	// Append profile ID
	p.profileIDs = append(p.profileIDs, id)

	// First profile? No need to merge.
	if p.profile == nil {
		p.profile = prof
		return nil
	}

	// Merge profiles after the first one.
	p.profile, err = profile.Merge([]*profile.Profile{p.profile, prof})
	return
}

// ApplyNoInlineHack removes samples that lead to bad inlining decisions.
func (p *MergedProfile) ApplyNoInlineHack() error {
	return ApplyNoInlineHack(p.profile)
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

// DebugQuery returns a query string that can be used to view the profiles that
// went into the merged profile.
func (p *MergedProfile) DebugQuery() string {
	return "profile-id:(" + strings.Join(p.profileIDs, " OR ") + ")"
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

// ProfilesDownload is the result of downloading several profiles from the pgo
// endpoint.
type ProfilesDownload struct {
	data []byte
}

// MergeProfile merges the profiles in the download into a single profile.
func (d *ProfilesDownload) MergedProfile(log *slog.Logger) (*MergedProfile, error) {
	zr, err := zip.NewReader(bytes.NewReader(d.data), int64(len(d.data)))
	if err != nil {
		return nil, err
	}

	var pgoProfile = &MergedProfile{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		prof, err := profile.Parse(rc)
		if err != nil {
			return nil, err
		}
		if err := pgoProfile.Merge(f.Name, prof); err != nil {
			return nil, err
		}

		seconds := prof.TimeNanos / int64(time.Second)
		nanoseconds := prof.TimeNanos % int64(time.Second)
		t := time.Unix(seconds, nanoseconds)

		cores, err := cpuCores(prof)
		if err != nil {
			log.Warn("failed to extract cpu cores", "error", err)
		}

		log.Info(
			"extracted profile",
			// "service", p.Service, TODO: can we get this?
			"cpu-cores", float64(int(cores*10))/10,
			"duration", time.Duration(prof.DurationNanos),
			"age", time.Since(t).Round(time.Second),
			"profile-id", f.Name,
		)
		if err := rc.Close(); err != nil {
			return nil, err
		}
	}

	return pgoProfile, nil
}

// cpuCores returns the number of CPU cores used in the profile.
func cpuCores(prof *profile.Profile) (float64, error) {
	cpuIdx := -1
	for idx, st := range prof.SampleType {
		if st.Type == "cpu" && st.Unit == "nanoseconds" {
			cpuIdx = idx
			break
		}
	}
	if cpuIdx == -1 {
		return 0, errors.New("no cpu sample type found")
	}
	var cpuNanos int64
	for _, s := range prof.Sample {
		if len(s.Value) <= int(cpuIdx) {
			return 0, errors.New("invalid sample value")
		}
		cpuNanos += s.Value[cpuIdx]
	}
	return float64(cpuNanos) / float64(prof.DurationNanos), nil
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
