# datadog-pgo 🚀

datadog-pgo is a tool for integrating continuous [profile-guided optimization](https://go.dev/doc/pgo) (PGO) into your Go build process. It fetches representative CPU profiles from Datadog and merges them into a `default.pgo` file that is used by the Go toolchain to optimize your application.

You can learn more about this feature in our [official documentation](https://docs.datadoghq.com/profiler/guide/save-cpu-in-production-with-go-pgo/) as well as in [our announcement blog post](https://www.datadoghq.com/blog/datadog-pgo-go/).


## Getting Started

1. Create a dedicated API key and unscoped Application key for PGO as described in the [documentation](https://docs.datadoghq.com/account_management/api-app-keys/).
2. Set `DD_API_KEY`, `DD_APP_KEY` and `DD_SITE` via the environment secret mechanism of your CI provider.
3. Run `datadog-pgo` before your build step. E.g. for a service `foo` that runs in `prod` and has its main package in `./cmd/foo`, you should add this step:

```
go run github.com/DataDog/datadog-pgo@latest "service:foo env:prod" ./cmd/foo/default.pgo
```

That's it. The go toolchain will automatically pick up any `default.pgo` file in the main package, so there is no need to modify your `go build` step.

Note: You should always use the `datadog-pgo@latest` version as shown above. Old versions may become deprecated and stop working on short notice.


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
code in order to let your build succeed, even if a PGO download error occured.

OPTIONS
  -fail
    	return with a non-zero exit code on failure
  -from duration
    	how far back to search for profiles (default 72h0m0s)
  -json
    	print logs in json format
  -profiles int
    	the number of profiles to fetch per query (default 5)
  -timeout duration
    	timeout for fetching PGO profile (default 1m0s)
  -v	verbose output
```
<!-- scripts/update_readme.go -->

## FAQ

### How are profiles selected?

By default datadog-pgo selects the top 5 profiles by CPU utilization within the last 72 hours. You can change this behavior using the `-profiles` and `-from` flags.

This oppinionated approach is based on our internal testing where it has yielded better results than taking a sample of average profiles.

Please open a GitHub issue if you have feedback on this.

### Can I use profiles from a different environment?

The official [PGO documentation](https://go.dev/doc/pgo) recommends using profiles from your production environment. Profiles from other environments may not be representative of the production workload and will likely yield suboptimal results.

### How do I know if PGO is working?

dd-trace-go tags the profiles of PGO-enabled applications with a `pgo:true` tag. You can search for profiles with this tag in the profile explorer.

### How can I measure the impact of PGO?

The impact of PGO can be tricky to measure. When in doubt, try to measure CPU time per request by building a dashboard widget that divides the CPU usage of your application by the number of requests it serves. We hope to provide a better solution for this in the future.

### What happens if there is a problem?

datadog-pgo will always return with a zero exit code in order to let your build succeed, even if pgo downloading failed. If you want to fail the build on error, use the `-fail` flag.

### How can I look at the profiles?

1. Copy the the `debug-query` output from the last log line of datadog-pgo.
2. Go to the profile explorer in the Datadog UI and paste the query.
3. Increase the time range to 1 week to make sure you see all profiles.

Please note that the profile retention is 7 days. If you're interested in the use case of retaining pgo profiles for longer, please let us know by opening an github issue on this repo.

### How can I provide feedback?

Just open a GitHub issue on this repository. We're happy to hear from you!
