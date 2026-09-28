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
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"go.yaml.in/yaml/v2"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	"github.com/prometheus/prometheus/util/logging"
)

// String constants for instrumentation.
const (
	namespace  = "prometheus"
	subsystem  = "remote_storage"
	remoteName = "remote_name"
	endpoint   = "url"
)

type ReadyScrapeManager interface {
	Get() (*scrape.Manager, error)
}

// startTimeCallback is a callback func that return the oldest timestamp stored in a storage.
type startTimeCallback func() (int64, error)

// Storage represents all the remote read and write endpoints.  It implements
// storage.Storage.
type Storage struct {
	deduper *logging.Deduper
	logger  *slog.Logger
	mtx     sync.Mutex

	rws *WriteStorage

	// For reads.
	queryables             []storage.SampleAndChunkQueryable
	requiredQueryables     []storage.SampleAndChunkQueryable
	localStartTimeCallback startTimeCallback
}

var (
	_ storage.Storage           = &Storage{}
	_ storage.RequiredQueryable = &Storage{}
)

// NewStorage returns a remote.Storage.
func NewStorage(l *slog.Logger, reg prometheus.Registerer, stCallback startTimeCallback, walDir string, flushDeadline time.Duration, sm ReadyScrapeManager, enableTypeAndUnitLabels bool) *Storage {
	if l == nil {
		l = promslog.NewNopLogger()
	}
	deduper := logging.Dedupe(l, 1*time.Minute)
	logger := slog.New(deduper)

	s := &Storage{
		logger:                 logger,
		deduper:                deduper,
		localStartTimeCallback: stCallback,
	}
	s.rws = NewWriteStorage(s.logger, reg, walDir, flushDeadline, sm, enableTypeAndUnitLabels)
	return s
}

func (s *Storage) Notify() {
	s.rws.Notify()
}

// ApplyConfig updates the state as the new config requires.
func (s *Storage) ApplyConfig(conf *config.Config) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if err := s.rws.ApplyConfig(conf); err != nil {
		return err
	}

	// Update read clients
	readHashes := make(map[string]struct{})
	var queryables, requiredQueryables []storage.SampleAndChunkQueryable
	for _, rrConf := range conf.RemoteReadConfigs {
		hash, err := toHash(rrConf)
		if err != nil {
			return err
		}

		// Don't allow duplicate remote read configs.
		if _, ok := readHashes[hash]; ok {
			return fmt.Errorf("duplicate remote read configs are not allowed, found duplicate for URL: %s", rrConf.URL)
		}
		readHashes[hash] = struct{}{}

		// Set the queue name to the config hash if the user has not set
		// a name in their remote write config so we can still differentiate
		// between queues that have the same remote write endpoint.
		name := hash[:6]
		if rrConf.Name != "" {
			name = rrConf.Name
		}

		c, err := NewReadClient(name, &ClientConfig{
			URL:              rrConf.URL,
			Timeout:          rrConf.RemoteTimeout,
			ChunkedReadLimit: rrConf.ChunkedReadLimit,
			HTTPClientConfig: rrConf.HTTPClientConfig,
			Headers:          rrConf.Headers,
		})
		if err != nil {
			return err
		}

		externalLabels := conf.GlobalConfig.ExternalLabels
		if !rrConf.FilterExternalLabels {
			externalLabels = labels.EmptyLabels()
		}
		q := NewSampleAndChunkQueryableClient(
			c,
			externalLabels,
			labelsToEqualityMatchers(rrConf.RequiredMatchers),
			rrConf.ReadRecent,
			s.localStartTimeCallback,
		)
		if rrConf.Required {
			requiredQueryables = append(requiredQueryables, q)
		} else {
			queryables = append(queryables, q)
		}
	}
	s.queryables = queryables
	s.requiredQueryables = requiredQueryables

	return nil
}

// StartTime implements the Storage interface.
func (*Storage) StartTime() (int64, error) {
	return int64(model.Latest), nil
}

// Querier returns a storage.MergeQuerier combining the remote client queriers
// of each remote read endpoint that is not configured as required.
// Returned querier will never return error as all queryables are assumed best effort.
// Additionally all returned queriers ensure that its Select's SeriesSets have ready data after first `Next` invoke.
// This is because Prometheus (fanout and secondary queries) can't handle the stream failing half way through by design.
func (s *Storage) Querier(mint, maxt int64) (storage.Querier, error) {
	s.mtx.Lock()
	queryables := s.queryables
	s.mtx.Unlock()

	queriers := make([]storage.Querier, 0, len(queryables))
	for _, queryable := range queryables {
		q, err := queryable.Querier(mint, maxt)
		if err != nil {
			return nil, err
		}
		queriers = append(queriers, q)
	}
	return storage.NewMergeQuerier(nil, queriers, storage.ChainedSeriesMerge), nil
}

// ChunkQuerier returns a storage.MergeQuerier combining the remote client queriers
// of each remote read endpoint that is not configured as required.
func (s *Storage) ChunkQuerier(mint, maxt int64) (storage.ChunkQuerier, error) {
	s.mtx.Lock()
	queryables := s.queryables
	s.mtx.Unlock()

	queriers := make([]storage.ChunkQuerier, 0, len(queryables))
	for _, queryable := range queryables {
		q, err := queryable.ChunkQuerier(mint, maxt)
		if err != nil {
			return nil, err
		}
		queriers = append(queriers, q)
	}
	return storage.NewMergeChunkQuerier(nil, queriers, storage.NewCompactingChunkSeriesMerger(storage.ChainedSeriesMerge)), nil
}

// RequiredQuerier implements storage.RequiredQueryable. It returns a querier
// combining the remote read endpoints configured as required, whose Select
// errors are returned as errors rather than warnings.
func (s *Storage) RequiredQuerier(mint, maxt int64) (storage.Querier, error) {
	s.mtx.Lock()
	queryables := s.requiredQueryables
	s.mtx.Unlock()

	queriers := make([]storage.Querier, 0, len(queryables))
	for _, queryable := range queryables {
		q, err := queryable.Querier(mint, maxt)
		if err != nil {
			return nil, err
		}
		if q != storage.NoopQuerier() {
			q = requiredQuerier{q}
		}
		queriers = append(queriers, q)
	}
	return storage.NewMergeQuerier(queriers, nil, storage.ChainedSeriesMerge), nil
}

// RequiredChunkQuerier implements storage.RequiredQueryable. It returns a chunk
// querier combining the remote read endpoints configured as required, whose
// Select errors are returned as errors rather than warnings.
func (s *Storage) RequiredChunkQuerier(mint, maxt int64) (storage.ChunkQuerier, error) {
	s.mtx.Lock()
	queryables := s.requiredQueryables
	s.mtx.Unlock()

	queriers := make([]storage.ChunkQuerier, 0, len(queryables))
	for _, queryable := range queryables {
		q, err := queryable.ChunkQuerier(mint, maxt)
		if err != nil {
			return nil, err
		}
		if q != storage.NoopChunkedQuerier() {
			q = requiredChunkQuerier{q}
		}
		queriers = append(queriers, q)
	}
	return storage.NewMergeChunkQuerier(queriers, nil, storage.NewCompactingChunkSeriesMerger(storage.ChainedSeriesMerge)), nil
}

// requiredQuerier returns Select errors of a required remote read endpoint as
// errors, but keeps label lookups best effort, since remote read does not
// support them.
type requiredQuerier struct {
	storage.Querier
}

func (q requiredQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return labelsAsWarnings(q.Querier.LabelValues(ctx, name, hints, matchers...))
}

func (q requiredQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return labelsAsWarnings(q.Querier.LabelNames(ctx, hints, matchers...))
}

// requiredChunkQuerier is the storage.ChunkQuerier equivalent of requiredQuerier.
type requiredChunkQuerier struct {
	storage.ChunkQuerier
}

func (q requiredChunkQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return labelsAsWarnings(q.ChunkQuerier.LabelValues(ctx, name, hints, matchers...))
}

func (q requiredChunkQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return labelsAsWarnings(q.ChunkQuerier.LabelNames(ctx, hints, matchers...))
}

func labelsAsWarnings(vals []string, ws annotations.Annotations, err error) ([]string, annotations.Annotations, error) {
	if err != nil {
		return nil, ws.Add(err), nil
	}
	return vals, ws, nil
}

// Appender implements storage.Storage.
func (s *Storage) Appender(ctx context.Context) storage.Appender {
	return s.rws.Appender(ctx)
}

// AppenderV2 implements storage.Storage.
func (s *Storage) AppenderV2(ctx context.Context) storage.AppenderV2 {
	return s.rws.AppenderV2(ctx)
}

// LowestSentTimestamp returns the lowest sent timestamp across all queues.
func (s *Storage) LowestSentTimestamp() int64 {
	return s.rws.LowestSentTimestamp()
}

// Close the background processing of the storage queues.
func (s *Storage) Close() error {
	s.deduper.Stop()
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.rws.Close()
}

func labelsToEqualityMatchers(ls model.LabelSet) []*labels.Matcher {
	ms := make([]*labels.Matcher, 0, len(ls))
	for k, v := range ls {
		ms = append(ms, &labels.Matcher{
			Type:  labels.MatchEqual,
			Name:  string(k),
			Value: string(v),
		})
	}
	return ms
}

// Used for hashing configs and diff'ing hashes in ApplyConfig.
func toHash(data any) (string, error) {
	bytes, err := yaml.Marshal(data)
	if err != nil {
		return "", err
	}
	hash := md5.Sum(bytes)
	return hex.EncodeToString(hash[:]), nil
}
