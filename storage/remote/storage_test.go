// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	common_config "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	"github.com/prometheus/prometheus/util/teststorage"
)

func TestStorageLifecycle(t *testing.T) {
	dir := t.TempDir()

	s := NewStorage(nil, nil, nil, dir, defaultFlushDeadline, nil, false)
	conf := &config.Config{
		GlobalConfig: config.DefaultGlobalConfig,
		RemoteWriteConfigs: []*config.RemoteWriteConfig{
			// We need to set URL's so that metric creation doesn't panic.
			baseRemoteWriteConfig("http://test-storage.com"),
		},
		RemoteReadConfigs: []*config.RemoteReadConfig{
			baseRemoteReadConfig("http://test-storage.com"),
		},
	}

	require.NoError(t, s.ApplyConfig(conf))

	// make sure remote write has a queue.
	require.Len(t, s.rws.queues, 1)

	// make sure remote write has a queue.
	require.Len(t, s.queryables, 1)

	err := s.Close()
	require.NoError(t, err)
}

func TestUpdateRemoteReadConfigs(t *testing.T) {
	dir := t.TempDir()

	s := NewStorage(nil, nil, nil, dir, defaultFlushDeadline, nil, false)

	conf := &config.Config{
		GlobalConfig: config.GlobalConfig{},
	}
	require.NoError(t, s.ApplyConfig(conf))
	require.Empty(t, s.queryables)

	conf.RemoteReadConfigs = []*config.RemoteReadConfig{
		baseRemoteReadConfig("http://test-storage.com"),
	}
	require.NoError(t, s.ApplyConfig(conf))
	require.Len(t, s.queryables, 1)

	err := s.Close()
	require.NoError(t, err)
}

func TestFilterExternalLabels(t *testing.T) {
	dir := t.TempDir()

	s := NewStorage(nil, nil, nil, dir, defaultFlushDeadline, nil, false)

	conf := &config.Config{
		GlobalConfig: config.GlobalConfig{
			ExternalLabels: labels.FromStrings("foo", "bar"),
		},
	}
	require.NoError(t, s.ApplyConfig(conf))
	require.Empty(t, s.queryables)

	conf.RemoteReadConfigs = []*config.RemoteReadConfig{
		baseRemoteReadConfig("http://test-storage.com"),
	}

	require.NoError(t, s.ApplyConfig(conf))
	require.Len(t, s.queryables, 1)
	require.Equal(t, 1, s.queryables[0].(*sampleAndChunkQueryableClient).externalLabels.Len())

	err := s.Close()
	require.NoError(t, err)
}

func TestIgnoreExternalLabels(t *testing.T) {
	dir := t.TempDir()

	s := NewStorage(nil, nil, nil, dir, defaultFlushDeadline, nil, false)

	conf := &config.Config{
		GlobalConfig: config.GlobalConfig{
			ExternalLabels: labels.FromStrings("foo", "bar"),
		},
	}
	require.NoError(t, s.ApplyConfig(conf))
	require.Empty(t, s.queryables)

	conf.RemoteReadConfigs = []*config.RemoteReadConfig{
		baseRemoteReadConfig("http://test-storage.com"),
	}

	conf.RemoteReadConfigs[0].FilterExternalLabels = false

	require.NoError(t, s.ApplyConfig(conf))
	require.Len(t, s.queryables, 1)
	require.Equal(t, 0, s.queryables[0].(*sampleAndChunkQueryableClient).externalLabels.Len())

	err := s.Close()
	require.NoError(t, err)
}

func TestRequiredRemoteRead(t *testing.T) {
	remoteStore := teststorage.New(t)
	app := remoteStore.Appender(context.Background())
	_, err := app.Append(0, labels.FromStrings(model.MetricNameLabel, "up", "source", "remote"), 1000, 1)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	working := httptest.NewServer(NewReadHandler(nil, nil, remoteStore, func() config.Config { return config.Config{} }, 1e6, 1, 0))
	t.Cleanup(working.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	type endpoint struct {
		url              string
		required         bool
		readRecent       bool
		requiredMatchers model.LabelSet
	}
	cases := []struct {
		name          string
		endpoints     []endpoint
		expectErr     bool
		expectWarning bool
		expectSeries  int
	}{
		{
			name:          "failing best effort endpoint returns a warning",
			endpoints:     []endpoint{{url: failing.URL, readRecent: true}},
			expectWarning: true,
		},
		{
			name:      "failing required endpoint fails the query",
			endpoints: []endpoint{{url: failing.URL, required: true, readRecent: true}},
			expectErr: true,
		},
		{
			name:         "working required endpoint returns its series",
			endpoints:    []endpoint{{url: working.URL, required: true, readRecent: true}},
			expectSeries: 1,
		},
		{
			name: "failing best effort endpoint next to working required endpoint",
			endpoints: []endpoint{
				{url: failing.URL, readRecent: true},
				{url: working.URL, required: true, readRecent: true},
			},
			expectWarning: true,
			expectSeries:  1,
		},
		{
			name: "failing required endpoint next to working best effort endpoint",
			endpoints: []endpoint{
				{url: working.URL, readRecent: true},
				{url: failing.URL, required: true, readRecent: true},
			},
			expectErr: true,
		},
		{
			name:      "required endpoint skipped by required_matchers",
			endpoints: []endpoint{{url: failing.URL, required: true, readRecent: true, requiredMatchers: model.LabelSet{"job": "special"}}},
		},
		{
			name:      "required endpoint skipped as local storage covers the time range",
			endpoints: []endpoint{{url: failing.URL, required: true}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Local storage starts at 0, so without read_recent queries after 0 are served locally only.
			rs := NewStorage(nil, nil, func() (int64, error) { return 0, nil }, t.TempDir(), defaultFlushDeadline, nil, false)
			t.Cleanup(func() { require.NoError(t, rs.Close()) })

			conf := &config.Config{GlobalConfig: config.DefaultGlobalConfig}
			for _, e := range tc.endpoints {
				rrConf := baseRemoteReadConfig(e.url)
				rrConf.Required = e.required
				rrConf.ReadRecent = e.readRecent
				rrConf.RequiredMatchers = e.requiredMatchers
				conf.RemoteReadConfigs = append(conf.RemoteReadConfigs, rrConf)
			}
			require.NoError(t, rs.ApplyConfig(conf))

			fanout := storage.NewFanout(nil, teststorage.New(t), rs)
			matcher := labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "up")

			check := func(t *testing.T, series int, err error, ws annotations.Annotations) {
				t.Helper()
				if tc.expectErr {
					require.ErrorContains(t, err, "remote_read")
					return
				}
				require.NoError(t, err)
				require.Equal(t, tc.expectSeries, series)
				if tc.expectWarning {
					require.Len(t, ws, 1)
					require.ErrorContains(t, ws.AsErrors()[0], "remote_read")
				} else {
					require.Empty(t, ws)
				}
			}

			t.Run("samples", func(t *testing.T) {
				q, err := fanout.Querier(100, 2000)
				require.NoError(t, err)
				defer q.Close()

				ss := q.Select(context.Background(), true, nil, matcher)
				series := 0
				for ss.Next() {
					series++
				}
				check(t, series, ss.Err(), ss.Warnings())

				// Remote read does not support label lookups, so they stay best effort.
				_, _, err = q.LabelNames(context.Background(), nil)
				require.NoError(t, err)
				_, _, err = q.LabelValues(context.Background(), "source", nil)
				require.NoError(t, err)
			})
			t.Run("chunks", func(t *testing.T) {
				q, err := fanout.ChunkQuerier(100, 2000)
				require.NoError(t, err)
				defer q.Close()

				ss := q.Select(context.Background(), true, nil, matcher)
				series := 0
				for ss.Next() {
					series++
				}
				check(t, series, ss.Err(), ss.Warnings())

				_, _, err = q.LabelNames(context.Background(), nil)
				require.NoError(t, err)
				_, _, err = q.LabelValues(context.Background(), "source", nil)
				require.NoError(t, err)
			})
		})
	}
}

// mustURLParse parses a URL and panics on error.
func mustURLParse(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("failed to parse URL %q: %v", rawURL, err))
	}
	return u
}

// baseRemoteWriteConfig copy values from global Default Write config
// to avoid change global state and cross impact test execution.
func baseRemoteWriteConfig(host string) *config.RemoteWriteConfig {
	cfg := config.DefaultRemoteWriteConfig
	cfg.URL = &common_config.URL{
		URL: mustURLParse(host),
	}
	return &cfg
}

// baseRemoteReadConfig copy values from global Default Read config
// to avoid change global state and cross impact test execution.
func baseRemoteReadConfig(host string) *config.RemoteReadConfig {
	cfg := config.DefaultRemoteReadConfig
	cfg.URL = &common_config.URL{
		URL: mustURLParse(host),
	}
	return &cfg
}

// TestWriteStorageApplyConfigsDuringCommit helps detecting races when
// ApplyConfig runs concurrently with Notify
// See https://github.com/prometheus/prometheus/issues/12747
func TestWriteStorageApplyConfigsDuringCommit(t *testing.T) {
	s := NewStorage(nil, nil, nil, t.TempDir(), defaultFlushDeadline, nil, false)

	var wg sync.WaitGroup
	wg.Add(2000)

	start := make(chan struct{})
	for i := range 1000 {
		go func(i int) {
			<-start
			conf := &config.Config{
				GlobalConfig: config.DefaultGlobalConfig,
				RemoteWriteConfigs: []*config.RemoteWriteConfig{
					baseRemoteWriteConfig(fmt.Sprintf("http://test-%d.com", i)),
				},
			}
			require.NoError(t, s.ApplyConfig(conf))
			wg.Done()
		}(i)
	}

	for range 1000 {
		go func() {
			<-start
			s.Notify()
			wg.Done()
		}()
	}

	close(start)
	wg.Wait()
}
