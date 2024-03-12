package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// maxConcurrency is the maximum number of concurrent requests to make to the
// Datadog API.
const maxConcurrency = 5

// ClientFromEnv returns a new Client with its fields populated from the
// environment. It returns an error if any of the required environment variables
// are not set.
func ClientFromEnv() (*Client, error) {
	c := &Client{concurrency: make(chan struct{}, maxConcurrency)}
	if c.site = os.Getenv("DD_SITE"); c.site == "" {
		c.site = "datadoghq.com"
	}
	if c.apiKey = os.Getenv("DD_API_KEY"); c.apiKey == "" {
		return nil, errors.New("DD_API_KEY is not set")
	}
	if c.appKey = os.Getenv("DD_APP_KEY"); c.appKey == "" {
		return nil, errors.New("DD_APP_KEY is not set")
	}
	return c, nil
}

// Client is a client for the Datadog API.
type Client struct {
	site        string
	apiKey      string
	appKey      string
	concurrency chan struct{}
}

// SearchAndDownloadProfiles searches for profiles using the given queries and
// downloads them.
func (c *Client) SearchAndDownloadProfiles(ctx context.Context, queries []SearchQuery) (profiles *ProfilesDownload, err error) {
	defer wrapErr(&err, "search and download profiles")
	defer c.limitConcurrency()()

	var payload = struct {
		Queries []SearchQuery `json:"queries"`
	}{queries}

	data, err := c.post(ctx, "/api/unstable/profiles/gopgo", payload)
	if err != nil {
		return nil, err
	}
	return &ProfilesDownload{data: data}, nil
}

// SearchProfiles searches for profiles using the given query. It returns a list
// of profiles and an error if any.
func (c *Client) SearchProfiles(ctx context.Context, query SearchQuery) (profiles []*SearchProfile, err error) {
	defer wrapErr(&err, "search profiles")
	defer c.limitConcurrency()()
	var response struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				ID            string   `json:"id"`
				Service       string   `json:"service"`
				DurationNanos float64  `json:"duration_nanos"`
				Timestamp     JSONTime `json:"timestamp"`
				Custom        struct {
					Metrics struct {
						CoreCPUCores float64 `json:"core_cpu_cores"`
					} `json:"metrics"`
				} `json:"custom"`
			} `json:"attributes"`
		} `json:"data"`
	}
	data, err := c.post(ctx, "/api/unstable/profiles/list", query)
	if err != nil {
		return nil, err
	} else if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}

	if len(response.Data) == 0 {
		return nil, errors.New("no profiles found")
	}

	for _, item := range response.Data {
		p := &SearchProfile{
			EventID:   item.ID,
			ProfileID: item.Attributes.ID,
			Service:   item.Attributes.Service,
			CPUCores:  item.Attributes.Custom.Metrics.CoreCPUCores,
			Timestamp: item.Attributes.Timestamp.Time,
			Duration:  time.Duration(item.Attributes.DurationNanos),
		}
		profiles = append(profiles, p)
	}
	return
}

// DownloadProfile downloads the profile identified by the given SearchProfile.
func (c *Client) DownloadProfile(ctx context.Context, p *SearchProfile) (d ProfileDownload, err error) {
	defer wrapErr(&err, "download profile")
	defer c.limitConcurrency()()
	req, err := c.request(ctx, "GET", fmt.Sprintf("/api/ui/profiling/profiles/%s/download?eventId=%s", p.ProfileID, p.EventID), nil)
	if err != nil {
		return ProfileDownload{}, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ProfileDownload{}, err
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		return ProfileDownload{}, err
	}
	return ProfileDownload{data: data}, nil
}

// request creates a new HTTP request with the given method and path and sets
// the required headers.
func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	url := fmt.Sprintf("https://app.%s%s", c.site, path)

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", name+"/"+version)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("DD-API-KEY", c.apiKey)
	return req, nil
}

// post sends a POST request to the given path with the given payload and decodes
// the response.
func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := c.request(ctx, "POST", path, reqBody)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s: %s: please check that your DD_API_KEY, DD_APP_KEY and DD_SITE env vars are set correctly", path, res.Status)
	}
	return resBody, nil
}

// limitConcurrency blocks until a slot is available in the concurrency channel.
// It returns a function that should be called to release the slot.
func (c *Client) limitConcurrency() func() {
	c.concurrency <- struct{}{}
	return func() { <-c.concurrency }
}

// SearchQuery holds the query parameters for searching for profiles.
type SearchQuery struct {
	Filter SearchFilter `json:"filter"`
	Sort   SearchSort   `json:"sort"`
	Limit  int          `json:"limit"`
}

// SearchFilter holds the filter parameters for searching for profiles.
type SearchFilter struct {
	From  JSONTime `json:"from"`
	To    JSONTime `json:"to"`
	Query string   `json:"query"`
}

// SearchSort holds the sort parameters for searching for profiles.
type SearchSort struct {
	Order string `json:"order"`
	Field string `json:"field"`
}

// timeFormat is the time format used by the Datadog API.
const timeFormat = "2006-01-02T15:04:05.999999999Z"

// JSONTime is a time.Time that marshals to and from JSON in the format used by
// the Datadog API.
type JSONTime struct {
	time.Time
}

// MarshalJSON marshals the time in the format used by the Datadog API.
func (t JSONTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// UnmarshalJSON unmarshals the time from the format used by the Datadog API.
func (t *JSONTime) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(timeFormat, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// String returns the time in the format used by the Datadog API.
func (t JSONTime) String() string {
	return t.Time.UTC().Round(time.Second).Format(timeFormat)
}

// SearchProfile holds information about a profile search result. ProfileID and
// EventID are used to identify the SearchProfile for downloading. The other
// fields are just logged for debugging.
type SearchProfile struct {
	Service   string
	CPUCores  float64
	ProfileID string
	EventID   string
	Timestamp time.Time
	Duration  time.Duration
}
