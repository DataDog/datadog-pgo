# datadog-pgo

datadog-pgo is a tool for integrating continuous [profile-guided optimization](https://go.dev/doc/pgo) (PGO) into your Go build process. It fetches representative CPU profiles from Datadog and merges them into a `default.pgo` file that is used by the Go toolchain to optimize your application.

## Getting Started

1. Create a dedicated API key and unscoped Application key for PGO as described in the [documentation](https://docs.datadoghq.com/account_management/api-app-keys/).
2. Set the `DD_API_KEY` and `DD_APP_KEY` via the environment secret mechanism of your CI provider.
3. Add a step to your CI pipeline to download a `default.pgo` into the main package of your application. This step must run before your build step.

```
go run github.com/DataDog/datadog-pgo@latest 'service:foo env:prod' ./cmd/foo/default.pgo
```

**Public Beta:** Please always use the latest version of datadog-pgo in CI. Old versions may become deprecated and stop working on short notice.

## CLI

<!-- scripts/update_readme.go -->
```
usage: datadog-pgo [OPTIONS]... QUERY... DEST

datadog-pgo fetches CPU profiles from Datadog using the given QUERY arguments
and merges the results into a single DEST file suitable for profile-guided
optimization.

In order to use this, you need to set the following environment variables.

	DD_API_KEY: A Datadog API key
	DD_APP_KEY: A Datadog Application key
	DD_SITE: A Datadog site to use (defaults to datadoghq.com)

After this, typical usage will look like this:

	datadog-pgo 'service:my-service env:prod' ./cmd/my-service/default.pgo

The go toolchain will automatically pick up any default.pgo file found in the
main package (go1.21+), so you can build your service as usual, for example:

	go build ./cmd/my-service

Unless the -fail flag is set, datadog-pgo will always return with a zero exit
code in order to let your build succeed, even if no pgo downloading failed.

OPTIONS
  -fail
    	return with a non-zero exit code on failure
  -json
    	print logs in json format
  -profiles int
    	the number of profiles to fetch per query (default 5)
  -timeout duration
    	timeout for fetching pgo profile (default 1m0s)
  -v	verbose output
  -window duration
    	how far back to search for profiles (default 72h0m0s)
```
<!-- scripts/update_readme.go -->
