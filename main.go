// Command datadog-pgo fetches representative CPU profiles from Datadog and
// merges them into a single .pgo file that the Go toolchain can pick up for
// profile-guided optimization (PGO).
//
// It is a thin wrapper around github.com/DataDog/datadog-pgo/pgo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/DataDog/datadog-pgo/pgo"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
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
		usage := `usage: ` + pgo.Name + ` [OPTIONS]... QUERY... DEST

` + pgo.Name + ` fetches CPU profiles from Datadog using the given QUERY arguments
and merges the results into a single DEST file suitable for profile-guided
optimization.

In order to use this, you need to set the following environment variables.

	DD_API_KEY: A Datadog API key
	DD_APP_KEY: A Datadog Application key
	DD_SITE: A Datadog site to use (defaults to datadoghq.com)

After this, typical usage will look like this:

	` + pgo.Name + ` 'service:my-service env:prod' ./cmd/my-service/default.pgo

The go toolchain will automatically pick up any default.pgo file found in the
main package (go1.21+), so you can build your service as usual, for example:

	go build ./cmd/my-service

Unless the -fail flag is set, ` + pgo.Name + ` will always return with a zero exit
code in order to let your build succeed, even if a PGO download error occured.

OPTIONS`
		fmt.Fprintln(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}

	// Parse flags
	var (
		failF     = flag.Bool("fail", false, "return with a non-zero exit code on failure")
		jsonF     = flag.Bool("json", false, "print logs in json format")
		profilesF = flag.Int("profiles", 5, "the number of profiles to fetch per query")
		timeoutF  = flag.Duration("timeout", 60*time.Second, "timeout for fetching PGO profile")
		verboseF  = flag.Bool("v", false, "verbose output")
		fromF     = flag.Duration("from", 3*24*time.Hour, "how far back to search for profiles")
	)
	flag.Parse()

	// Validate args
	if flag.NArg() < 2 {
		flag.Usage()
		return errors.New("at least 2 arguments are required")
	}

	// Split args into queries and dst
	queries := pgo.BuildQueries(*fromF, *profilesF, flag.Args()[:flag.NArg()-1])
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
	log.Info(pgo.Name, "version", pgo.Version, "go-version", runtime.Version())

	// Log errors and turn them into warnings unless -fail is set
	defer func() {
		if err == nil {
			return
		}
		log.Error(err.Error())
		err = loggedError{err}
		if !*failF {
			err = handledError{err}
			log.Warn(pgo.Name + " failed, but -fail is not set, returning exit code 0 to continue without PGO")
		}
	}()

	// Setup API client
	client, err := pgo.ClientFromEnv()
	if err != nil {
		return fmt.Errorf("clientFromEnv: %w", err)
	}

	// Create context
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutF)
	defer cancel()

	// Search, download and merge profiles
	mergedProfile, err := pgo.SearchDownloadMerge(ctx, log, client, queries)
	if err != nil {
		return err
	}

	// Apply no inline hack
	if err := mergedProfile.ApplyNoInlineHack(); err != nil {
		return err
	}

	// Writing pgo file to dst
	n, err := mergedProfile.Write(dst)
	if err != nil {
		return err
	}
	log.Info(
		"wrote PGO file",
		"path", dst,
		"samples", mergedProfile.Samples(),
		"bytes", n,
		"total-duration", timeSinceRoundMS(start),
		"debug-query", mergedProfile.DebugQuery(),
	)
	return nil
}

// timeSinceRoundMS returns the time since t rounded to the nearest millisecond.
func timeSinceRoundMS(t time.Time) time.Duration {
	return time.Since(t) / time.Millisecond * time.Millisecond
}

// loggedError is an error that has been logged.
type loggedError struct {
	error
}

// handledError is an error that has been handled.
type handledError struct {
	error
}
