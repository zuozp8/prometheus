// Copyright 2013 The Prometheus Authors
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
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/write/v2"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
)

const (
	// We track samples in/out and how long pushes take using an Exponentially
	// Weighted Moving Average.
	ewmaWeight          = 0.2
	shardUpdateDuration = 10 * time.Second

	// Allow 30% too many shards before scaling down.
	shardToleranceFraction = 0.3

	reasonTooOld                     = "too_old"
	reasonDroppedSeries              = "dropped_series"
	reasonUnintentionalDroppedSeries = "unintentionally_dropped_series"
)

type queueManagerMetrics struct {
	reg prometheus.Registerer

	samplesTotal           prometheus.Counter
	exemplarsTotal         prometheus.Counter
	histogramsTotal        prometheus.Counter
	metadataTotal          prometheus.Counter
	failedSamplesTotal     prometheus.Counter
	failedExemplarsTotal   prometheus.Counter
	failedHistogramsTotal  prometheus.Counter
	failedMetadataTotal    prometheus.Counter
	retriedSamplesTotal    prometheus.Counter
	retriedExemplarsTotal  prometheus.Counter
	retriedHistogramsTotal prometheus.Counter
	retriedMetadataTotal   prometheus.Counter
	droppedSamplesTotal    *prometheus.CounterVec
	droppedExemplarsTotal  *prometheus.CounterVec
	droppedHistogramsTotal *prometheus.CounterVec
	enqueueRetriesTotal    prometheus.Counter
	sentBatchDuration      prometheus.Histogram
	highestSentTimestamp   *maxTimestamp
	pendingSamples         prometheus.Gauge
	pendingExemplars       prometheus.Gauge
	pendingHistograms      prometheus.Gauge
	shardCapacity          prometheus.Gauge
	numShards              prometheus.Gauge
	maxNumShards           prometheus.Gauge
	minNumShards           prometheus.Gauge
	desiredNumShards       prometheus.Gauge
	sentBytesTotal         prometheus.Counter
	metadataBytesTotal     prometheus.Counter
	maxSamplesPerSend      prometheus.Gauge
}

func newQueueManagerMetrics(r prometheus.Registerer, rn, e string) *queueManagerMetrics {
	m := &queueManagerMetrics{
		reg: r,
	}
	constLabels := prometheus.Labels{
		remoteName: rn,
		endpoint:   e,
	}

	m.samplesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "samples_total",
		Help:        "Total number of samples sent to remote storage.",
		ConstLabels: constLabels,
	})
	m.exemplarsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "exemplars_total",
		Help:        "Total number of exemplars sent to remote storage.",
		ConstLabels: constLabels,
	})
	m.histogramsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "histograms_total",
		Help:        "Total number of histograms sent to remote storage.",
		ConstLabels: constLabels,
	})
	m.metadataTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "metadata_total",
		Help:        "Total number of metadata entries sent to remote storage.",
		ConstLabels: constLabels,
	})
	m.failedSamplesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "samples_failed_total",
		Help:        "Total number of samples which failed on send to remote storage, non-recoverable errors.",
		ConstLabels: constLabels,
	})
	m.failedExemplarsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "exemplars_failed_total",
		Help:        "Total number of exemplars which failed on send to remote storage, non-recoverable errors.",
		ConstLabels: constLabels,
	})
	m.failedHistogramsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "histograms_failed_total",
		Help:        "Total number of histograms which failed on send to remote storage, non-recoverable errors.",
		ConstLabels: constLabels,
	})
	m.failedMetadataTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "metadata_failed_total",
		Help:        "Total number of metadata entries which failed on send to remote storage, non-recoverable errors.",
		ConstLabels: constLabels,
	})
	m.retriedSamplesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "samples_retried_total",
		Help:        "Total number of samples which failed on send to remote storage but were retried because the send error was recoverable.",
		ConstLabels: constLabels,
	})
	m.retriedExemplarsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "exemplars_retried_total",
		Help:        "Total number of exemplars which failed on send to remote storage but were retried because the send error was recoverable.",
		ConstLabels: constLabels,
	})
	m.retriedHistogramsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "histograms_retried_total",
		Help:        "Total number of histograms which failed on send to remote storage but were retried because the send error was recoverable.",
		ConstLabels: constLabels,
	})
	m.retriedMetadataTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "metadata_retried_total",
		Help:        "Total number of metadata entries which failed on send to remote storage but were retried because the send error was recoverable.",
		ConstLabels: constLabels,
	})
	m.droppedSamplesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "samples_dropped_total",
		Help:        "Total number of samples which were dropped after being read from the WAL before being sent via remote write, either via relabelling, due to being too old or unintentionally because of an unknown reference ID.",
		ConstLabels: constLabels,
	}, []string{"reason"})
	m.droppedExemplarsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "exemplars_dropped_total",
		Help:        "Total number of exemplars which were dropped after being read from the WAL before being sent via remote write, either via relabelling, due to being too old or unintentionally because of an unknown reference ID.",
		ConstLabels: constLabels,
	}, []string{"reason"})
	m.droppedHistogramsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "histograms_dropped_total",
		Help:        "Total number of histograms which were dropped after being read from the WAL before being sent via remote write, either via relabelling, due to being too old or unintentionally because of an unknown reference ID.",
		ConstLabels: constLabels,
	}, []string{"reason"})
	m.enqueueRetriesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "enqueue_retries_total",
		Help:        "Total number of times enqueue has failed because a shards queue was full.",
		ConstLabels: constLabels,
	})
	m.sentBatchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "sent_batch_duration_seconds",
		Help:        "Duration of send calls to the remote storage.",
		Buckets:     append(prometheus.DefBuckets, 25, 60, 120, 300),
		ConstLabels: constLabels,
	})
	m.highestSentTimestamp = &maxTimestamp{
		Gauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Subsystem:   subsystem,
			Name:        "queue_highest_sent_timestamp_seconds",
			Help:        "Timestamp from a WAL sample, the highest timestamp successfully sent by this queue, in seconds since epoch.",
			ConstLabels: constLabels,
		}),
	}
	m.pendingSamples = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "samples_pending",
		Help:        "The number of samples pending in the queues shards to be sent to the remote storage.",
		ConstLabels: constLabels,
	})
	m.pendingExemplars = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "exemplars_pending",
		Help:        "The number of exemplars pending in the queues shards to be sent to the remote storage.",
		ConstLabels: constLabels,
	})
	m.pendingHistograms = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "histograms_pending",
		Help:        "The number of histograms pending in the queues shards to be sent to the remote storage.",
		ConstLabels: constLabels,
	})
	m.shardCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "shard_capacity",
		Help:        "The capacity of each shard of the queue used for parallel sending to the remote storage.",
		ConstLabels: constLabels,
	})
	m.numShards = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "shards",
		Help:        "The number of shards used for parallel sending to the remote storage.",
		ConstLabels: constLabels,
	})
	m.maxNumShards = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "shards_max",
		Help:        "The maximum number of shards that the queue is allowed to run.",
		ConstLabels: constLabels,
	})
	m.minNumShards = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "shards_min",
		Help:        "The minimum number of shards that the queue is allowed to run.",
		ConstLabels: constLabels,
	})
	m.desiredNumShards = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "shards_desired",
		Help:        "The number of shards that the queues shard calculation wants to run based on the rate of samples in vs. samples out.",
		ConstLabels: constLabels,
	})
	m.sentBytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "bytes_total",
		Help:        "The total number of bytes of data (not metadata) sent by the queue after compression. Note that when exemplars over remote write is enabled the exemplars included in a remote write request count towards this metric.",
		ConstLabels: constLabels,
	})
	m.metadataBytesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "metadata_bytes_total",
		Help:        "The total number of bytes of metadata sent by the queue after compression.",
		ConstLabels: constLabels,
	})
	m.maxSamplesPerSend = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "max_samples_per_send",
		Help:        "The maximum number of samples to be sent, in a single request, to the remote storage. Note that, when sending of exemplars over remote write is enabled, exemplars count towards this limt.",
		ConstLabels: constLabels,
	})

	return m
}

func (m *queueManagerMetrics) register() {
	if m.reg != nil {
		m.reg.MustRegister(
			m.samplesTotal,
			m.exemplarsTotal,
			m.histogramsTotal,
			m.metadataTotal,
			m.failedSamplesTotal,
			m.failedExemplarsTotal,
			m.failedHistogramsTotal,
			m.failedMetadataTotal,
			m.retriedSamplesTotal,
			m.retriedExemplarsTotal,
			m.retriedHistogramsTotal,
			m.retriedMetadataTotal,
			m.droppedSamplesTotal,
			m.droppedExemplarsTotal,
			m.droppedHistogramsTotal,
			m.enqueueRetriesTotal,
			m.sentBatchDuration,
			m.highestSentTimestamp,
			m.pendingSamples,
			m.pendingExemplars,
			m.pendingHistograms,
			m.shardCapacity,
			m.numShards,
			m.maxNumShards,
			m.minNumShards,
			m.desiredNumShards,
			m.sentBytesTotal,
			m.metadataBytesTotal,
			m.maxSamplesPerSend,
		)
	}
}

func (m *queueManagerMetrics) unregister() {
	if m.reg != nil {
		m.reg.Unregister(m.samplesTotal)
		m.reg.Unregister(m.exemplarsTotal)
		m.reg.Unregister(m.histogramsTotal)
		m.reg.Unregister(m.metadataTotal)
		m.reg.Unregister(m.failedSamplesTotal)
		m.reg.Unregister(m.failedExemplarsTotal)
		m.reg.Unregister(m.failedHistogramsTotal)
		m.reg.Unregister(m.failedMetadataTotal)
		m.reg.Unregister(m.retriedSamplesTotal)
		m.reg.Unregister(m.retriedExemplarsTotal)
		m.reg.Unregister(m.retriedHistogramsTotal)
		m.reg.Unregister(m.retriedMetadataTotal)
		m.reg.Unregister(m.droppedSamplesTotal)
		m.reg.Unregister(m.droppedExemplarsTotal)
		m.reg.Unregister(m.droppedHistogramsTotal)
		m.reg.Unregister(m.enqueueRetriesTotal)
		m.reg.Unregister(m.sentBatchDuration)
		m.reg.Unregister(m.highestSentTimestamp)
		m.reg.Unregister(m.pendingSamples)
		m.reg.Unregister(m.pendingExemplars)
		m.reg.Unregister(m.pendingHistograms)
		m.reg.Unregister(m.shardCapacity)
		m.reg.Unregister(m.numShards)
		m.reg.Unregister(m.maxNumShards)
		m.reg.Unregister(m.minNumShards)
		m.reg.Unregister(m.desiredNumShards)
		m.reg.Unregister(m.sentBytesTotal)
		m.reg.Unregister(m.metadataBytesTotal)
		m.reg.Unregister(m.maxSamplesPerSend)
	}
}

// WriteClient defines an interface for sending a batch of samples to an
// external timeseries database.
type WriteClient interface {
	// Store stores the given samples in the remote storage.
	Store(ctx context.Context, req []byte, retryAttempt int, rwFormat config.RemoteWriteFormat, compression string) error
	// Name uniquely identifies the remote storage.
	Name() string
	// Endpoint is the remote read or write endpoint for the storage client.
	Endpoint() string
	// Get the protocol versions supported by the endpoint.
	probeRemoteVersions(ctx context.Context) error
	// Get the last RW header received from the endpoint.
	GetLastRWHeader() string
}

const (
	Version1 config.RemoteWriteFormat = iota // 1.0, 0.1, etc.
	Version2                                 // symbols are indices into an array of strings.
)

// QueueManager manages a queue of samples to be sent to the Storage
// indicated by the provided WriteClient. Implements writeTo interface
// used by WAL Watcher.
type QueueManager struct {
	lastSendTimestamp          atomic.Int64
	buildRequestLimitTimestamp atomic.Int64

	logger               log.Logger
	flushDeadline        time.Duration
	cfg                  config.QueueConfig
	mcfg                 config.MetadataConfig
	externalLabels       []labels.Label
	relabelConfigs       []*relabel.Config
	sendExemplars        bool
	sendNativeHistograms bool
	watcher              *wlog.Watcher
	metadataWatcher      *MetadataWatcher
	rwFormat             config.RemoteWriteFormat

	clientMtx   sync.RWMutex
	storeClient WriteClient

	seriesMtx      sync.Mutex // Covers seriesLabels, seriesMetadata, droppedSeries and builder.
	seriesLabels   map[chunks.HeadSeriesRef]labels.Labels
	seriesMetadata map[chunks.HeadSeriesRef]*metadata.Metadata
	droppedSeries  map[chunks.HeadSeriesRef]struct{}
	builder        *labels.Builder

	seriesSegmentMtx     sync.Mutex // Covers seriesSegmentIndexes - if you also lock seriesMtx, take seriesMtx first.
	seriesSegmentIndexes map[chunks.HeadSeriesRef]int

	shards      *shards
	numShards   int
	reshardChan chan int
	quit        chan struct{}
	wg          sync.WaitGroup

	dataIn, dataDropped, dataOut, dataOutDuration *ewmaRate

	metrics              *queueManagerMetrics
	interner             *pool
	highestRecvTimestamp *maxTimestamp
}

// NewQueueManager builds a new QueueManager and starts a new
// WAL watcher with queue manager as the WriteTo destination.
// The WAL watcher takes the dir parameter as the base directory
// for where the WAL shall be located. Note that the full path to
// the WAL directory will be constructed as <dir>/wal.
func NewQueueManager(
	metrics *queueManagerMetrics,
	watcherMetrics *wlog.WatcherMetrics,
	readerMetrics *wlog.LiveReaderMetrics,
	logger log.Logger,
	dir string,
	samplesIn *ewmaRate,
	cfg config.QueueConfig,
	mCfg config.MetadataConfig,
	externalLabels labels.Labels,
	relabelConfigs []*relabel.Config,
	client WriteClient,
	flushDeadline time.Duration,
	interner *pool,
	highestRecvTimestamp *maxTimestamp,
	sm ReadyScrapeManager,
	enableExemplarRemoteWrite bool,
	enableNativeHistogramRemoteWrite bool,
	rwFormat config.RemoteWriteFormat,
) *QueueManager {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	// Copy externalLabels into a slice, which we need for processExternalLabels.
	extLabelsSlice := make([]labels.Label, 0, externalLabels.Len())
	externalLabels.Range(func(l labels.Label) {
		extLabelsSlice = append(extLabelsSlice, l)
	})

	logger = log.With(logger, remoteName, client.Name(), endpoint, client.Endpoint())
	t := &QueueManager{
		logger:               logger,
		flushDeadline:        flushDeadline,
		cfg:                  cfg,
		mcfg:                 mCfg,
		externalLabels:       extLabelsSlice,
		relabelConfigs:       relabelConfigs,
		storeClient:          client,
		sendExemplars:        enableExemplarRemoteWrite,
		sendNativeHistograms: enableNativeHistogramRemoteWrite,
		rwFormat:             rwFormat,

		seriesLabels:         make(map[chunks.HeadSeriesRef]labels.Labels),
		seriesMetadata:       make(map[chunks.HeadSeriesRef]*metadata.Metadata),
		seriesSegmentIndexes: make(map[chunks.HeadSeriesRef]int),
		droppedSeries:        make(map[chunks.HeadSeriesRef]struct{}),
		builder:              labels.NewBuilder(labels.EmptyLabels()),

		numShards:   cfg.MinShards,
		reshardChan: make(chan int),
		quit:        make(chan struct{}),

		dataIn:          samplesIn,
		dataDropped:     newEWMARate(ewmaWeight, shardUpdateDuration),
		dataOut:         newEWMARate(ewmaWeight, shardUpdateDuration),
		dataOutDuration: newEWMARate(ewmaWeight, shardUpdateDuration),

		metrics:              metrics,
		interner:             interner,
		highestRecvTimestamp: highestRecvTimestamp,
	}

	walMetadata := false
	if t.rwFormat > Version1 {
		walMetadata = true
	}
	t.watcher = wlog.NewWatcher(watcherMetrics, readerMetrics, logger, client.Name(), t, dir, enableExemplarRemoteWrite, enableNativeHistogramRemoteWrite, walMetadata)

	// The current MetadataWatcher implementation is mutually exclusive
	// with the new approach, which stores metadata as WAL records and
	// ships them alongside series. If both mechanisms are set, the new one
	// takes precedence by implicitly disabling the older one.
	if t.mcfg.Send && t.rwFormat > Version1 {
		level.Warn(logger).Log("msg", "usage of 'metadata_config.send' is redundant when using remote write v2 (or higher) as metadata will always be gathered from the WAL and included for every series within each write request")
		t.mcfg.Send = false
	}

	if t.mcfg.Send {
		t.metadataWatcher = NewMetadataWatcher(logger, sm, client.Name(), t, t.mcfg.SendInterval, flushDeadline)
	}
	t.shards = t.newShards()

	return t
}

// AppendWatcherMetadata sends metadata to the remote storage. Metadata is sent in batches, but is not parallelized.
// This is only used for the metadata_config.send setting and 1.x Remote Write.
func (t *QueueManager) AppendWatcherMetadata(ctx context.Context, metadata []scrape.MetricMetadata) {
	// no op for any newer proto format, which will cache metadata sent to it from the WAL watcher.
	if t.rwFormat > Version2 {
		return
	}
	// 1.X will still get metadata in batches.
	mm := make([]prompb.MetricMetadata, 0, len(metadata))
	for _, entry := range metadata {
		mm = append(mm, prompb.MetricMetadata{
			MetricFamilyName: entry.Metric,
			Help:             entry.Help,
			Type:             metricTypeToMetricTypeProto(entry.Type),
			Unit:             entry.Unit,
		})
	}

	pBuf := proto.NewBuffer(nil)
	numSends := int(math.Ceil(float64(len(metadata)) / float64(t.mcfg.MaxSamplesPerSend)))
	for i := 0; i < numSends; i++ {
		last := (i + 1) * t.mcfg.MaxSamplesPerSend
		if last > len(metadata) {
			last = len(metadata)
		}
		err := t.sendMetadataWithBackoff(ctx, mm[i*t.mcfg.MaxSamplesPerSend:last], pBuf)
		if err != nil {
			t.metrics.failedMetadataTotal.Add(float64(last - (i * t.mcfg.MaxSamplesPerSend)))
			level.Error(t.logger).Log("msg", "non-recoverable error while sending metadata", "count", last-(i*t.mcfg.MaxSamplesPerSend), "err", err)
		}
	}
}

func (t *QueueManager) sendMetadataWithBackoff(ctx context.Context, metadata []prompb.MetricMetadata, pBuf *proto.Buffer) error {
	// Build the WriteRequest with no samples.

	// Get compression to use from content negotiation based on last header seen (defaults to snappy).
	compression, _ := negotiateRWProto(t.rwFormat, t.storeClient.GetLastRWHeader())

	req, _, _, err := buildWriteRequest(t.logger, nil, metadata, pBuf, nil, nil, compression)
	if err != nil {
		return err
	}

	metadataCount := len(metadata)

	attemptStore := func(try int) error {
		ctx, span := otel.Tracer("").Start(ctx, "Remote Metadata Send Batch")
		defer span.End()

		span.SetAttributes(
			attribute.Int("metadata", metadataCount),
			attribute.Int("try", try),
			attribute.String("remote_name", t.storeClient.Name()),
			attribute.String("remote_url", t.storeClient.Endpoint()),
		)
		// Attributes defined by OpenTelemetry semantic conventions.
		if try > 0 {
			span.SetAttributes(semconv.HTTPResendCount(try))
		}

		begin := time.Now()
		err := t.storeClient.Store(ctx, req, try, Version1, compression)
		t.metrics.sentBatchDuration.Observe(time.Since(begin).Seconds())

		if err != nil {
			span.RecordError(err)
			return err
		}

		return nil
	}

	retry := func() {
		t.metrics.retriedMetadataTotal.Add(float64(len(metadata)))
	}
	err = sendWriteRequestWithBackoff(ctx, t.cfg, t.logger, attemptStore, retry)
	if err != nil {
		return err
	}
	t.metrics.metadataTotal.Add(float64(len(metadata)))
	t.metrics.metadataBytesTotal.Add(float64(len(req)))
	return nil
}

func isSampleOld(baseTime time.Time, sampleAgeLimit time.Duration, ts int64) bool {
	if sampleAgeLimit == 0 {
		// If sampleAgeLimit is unset, then we never skip samples due to their age.
		return false
	}
	limitTs := baseTime.Add(-sampleAgeLimit)
	sampleTs := timestamp.Time(ts)
	return sampleTs.Before(limitTs)
}

func isTimeSeriesOldFilter(metrics *queueManagerMetrics, baseTime time.Time, sampleAgeLimit time.Duration) func(ts prompb.TimeSeries) bool {
	return func(ts prompb.TimeSeries) bool {
		if sampleAgeLimit == 0 {
			// If sampleAgeLimit is unset, then we never skip samples due to their age.
			return false
		}
		switch {
		// Only the first element should be set in the series, therefore we only check the first element.
		case len(ts.Samples) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Samples[0].Timestamp) {
				metrics.droppedSamplesTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		case len(ts.Histograms) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Histograms[0].Timestamp) {
				metrics.droppedHistogramsTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		case len(ts.Exemplars) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Exemplars[0].Timestamp) {
				metrics.droppedExemplarsTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		default:
			return false
		}
		return false
	}
}

func isV2TimeSeriesOldFilter(metrics *queueManagerMetrics, baseTime time.Time, sampleAgeLimit time.Duration) func(ts writev2.TimeSeries) bool {
	return func(ts writev2.TimeSeries) bool {
		if sampleAgeLimit == 0 {
			// If sampleAgeLimit is unset, then we never skip samples due to their age.
			return false
		}
		switch {
		// Only the first element should be set in the series, therefore we only check the first element.
		case len(ts.Samples) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Samples[0].Timestamp) {
				metrics.droppedSamplesTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		case len(ts.Histograms) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Histograms[0].Timestamp) {
				metrics.droppedHistogramsTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		case len(ts.Exemplars) > 0:
			if isSampleOld(baseTime, sampleAgeLimit, ts.Exemplars[0].Timestamp) {
				metrics.droppedExemplarsTotal.WithLabelValues(reasonTooOld).Inc()
				return true
			}
		default:
			return false
		}
		return false
	}
}

// Append queues a sample to be sent to the remote storage. Blocks until all samples are
// enqueued on their shards or a shutdown signal is received.
func (t *QueueManager) Append(samples []record.RefSample) bool {
	currentTime := time.Now()
outer:
	for _, s := range samples {
		if isSampleOld(currentTime, time.Duration(t.cfg.SampleAgeLimit), s.T) {
			t.metrics.droppedSamplesTotal.WithLabelValues(reasonTooOld).Inc()
			continue
		}
		t.seriesMtx.Lock()
		lbls, ok := t.seriesLabels[s.Ref]
		if !ok {
			t.dataDropped.incr(1)
			if _, ok := t.droppedSeries[s.Ref]; !ok {
				level.Info(t.logger).Log("msg", "Dropped sample for series that was not explicitly dropped via relabelling", "ref", s.Ref)
				t.metrics.droppedSamplesTotal.WithLabelValues(reasonUnintentionalDroppedSeries).Inc()
			} else {
				t.metrics.droppedSamplesTotal.WithLabelValues(reasonDroppedSeries).Inc()
			}
			t.seriesMtx.Unlock()
			continue
		}
		// todo: handle or at least log an error if no metadata is found
		meta := t.seriesMetadata[s.Ref]
		t.seriesMtx.Unlock()
		// Start with a very small backoff. This should not be t.cfg.MinBackoff
		// as it can happen without errors, and we want to pickup work after
		// filling a queue/resharding as quickly as possible.
		// TODO: Consider using the average duration of a request as the backoff.
		backoff := model.Duration(5 * time.Millisecond)
		for {
			select {
			case <-t.quit:
				return false
			default:
			}
			if t.shards.enqueue(s.Ref, timeSeries{
				seriesLabels: lbls,
				metadata:     meta,
				timestamp:    s.T,
				value:        s.V,
				sType:        tSample,
			}) {
				continue outer
			}

			t.metrics.enqueueRetriesTotal.Inc()
			time.Sleep(time.Duration(backoff))
			backoff *= 2
			// It is reasonable to use t.cfg.MaxBackoff here, as if we have hit
			// the full backoff we are likely waiting for external resources.
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

func (t *QueueManager) AppendExemplars(exemplars []record.RefExemplar) bool {
	if !t.sendExemplars {
		return true
	}
	currentTime := time.Now()
outer:
	for _, e := range exemplars {
		if isSampleOld(currentTime, time.Duration(t.cfg.SampleAgeLimit), e.T) {
			t.metrics.droppedExemplarsTotal.WithLabelValues(reasonTooOld).Inc()
			continue
		}
		t.seriesMtx.Lock()
		lbls, ok := t.seriesLabels[e.Ref]
		if !ok {
			// Track dropped exemplars in the same EWMA for sharding calc.
			t.dataDropped.incr(1)
			if _, ok := t.droppedSeries[e.Ref]; !ok {
				level.Info(t.logger).Log("msg", "Dropped exemplar for series that was not explicitly dropped via relabelling", "ref", e.Ref)
				t.metrics.droppedExemplarsTotal.WithLabelValues(reasonUnintentionalDroppedSeries).Inc()
			} else {
				t.metrics.droppedExemplarsTotal.WithLabelValues(reasonDroppedSeries).Inc()
			}
			t.seriesMtx.Unlock()
			continue
		}
		meta := t.seriesMetadata[e.Ref]
		t.seriesMtx.Unlock()
		// This will only loop if the queues are being resharded.
		backoff := t.cfg.MinBackoff
		for {
			select {
			case <-t.quit:
				return false
			default:
			}
			if t.shards.enqueue(e.Ref, timeSeries{
				seriesLabels:   lbls,
				metadata:       meta,
				timestamp:      e.T,
				value:          e.V,
				exemplarLabels: e.Labels,
				sType:          tExemplar,
			}) {
				continue outer
			}

			t.metrics.enqueueRetriesTotal.Inc()
			time.Sleep(time.Duration(backoff))
			backoff *= 2
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

func (t *QueueManager) AppendHistograms(histograms []record.RefHistogramSample) bool {
	if !t.sendNativeHistograms {
		return true
	}
	currentTime := time.Now()
outer:
	for _, h := range histograms {
		if isSampleOld(currentTime, time.Duration(t.cfg.SampleAgeLimit), h.T) {
			t.metrics.droppedHistogramsTotal.WithLabelValues(reasonTooOld).Inc()
			continue
		}
		t.seriesMtx.Lock()
		lbls, ok := t.seriesLabels[h.Ref]
		if !ok {
			t.dataDropped.incr(1)
			if _, ok := t.droppedSeries[h.Ref]; !ok {
				level.Info(t.logger).Log("msg", "Dropped histogram for series that was not explicitly dropped via relabelling", "ref", h.Ref)
				t.metrics.droppedHistogramsTotal.WithLabelValues(reasonUnintentionalDroppedSeries).Inc()
			} else {
				t.metrics.droppedHistogramsTotal.WithLabelValues(reasonDroppedSeries).Inc()
			}
			t.seriesMtx.Unlock()
			continue
		}
		meta := t.seriesMetadata[h.Ref]
		t.seriesMtx.Unlock()

		backoff := model.Duration(5 * time.Millisecond)
		for {
			select {
			case <-t.quit:
				return false
			default:
			}
			if t.shards.enqueue(h.Ref, timeSeries{
				seriesLabels: lbls,
				metadata:     meta,
				timestamp:    h.T,
				histogram:    h.H,
				sType:        tHistogram,
			}) {
				continue outer
			}

			t.metrics.enqueueRetriesTotal.Inc()
			time.Sleep(time.Duration(backoff))
			backoff *= 2
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

func (t *QueueManager) AppendFloatHistograms(floatHistograms []record.RefFloatHistogramSample) bool {
	if !t.sendNativeHistograms {
		return true
	}
	currentTime := time.Now()
outer:
	for _, h := range floatHistograms {
		if isSampleOld(currentTime, time.Duration(t.cfg.SampleAgeLimit), h.T) {
			t.metrics.droppedHistogramsTotal.WithLabelValues(reasonTooOld).Inc()
			continue
		}
		t.seriesMtx.Lock()
		lbls, ok := t.seriesLabels[h.Ref]
		if !ok {
			t.dataDropped.incr(1)
			if _, ok := t.droppedSeries[h.Ref]; !ok {
				level.Info(t.logger).Log("msg", "Dropped histogram for series that was not explicitly dropped via relabelling", "ref", h.Ref)
				t.metrics.droppedHistogramsTotal.WithLabelValues(reasonUnintentionalDroppedSeries).Inc()
			} else {
				t.metrics.droppedHistogramsTotal.WithLabelValues(reasonDroppedSeries).Inc()
			}
			t.seriesMtx.Unlock()
			continue
		}
		meta := t.seriesMetadata[h.Ref]
		t.seriesMtx.Unlock()

		backoff := model.Duration(5 * time.Millisecond)
		for {
			select {
			case <-t.quit:
				return false
			default:
			}
			if t.shards.enqueue(h.Ref, timeSeries{
				seriesLabels:   lbls,
				metadata:       meta,
				timestamp:      h.T,
				floatHistogram: h.FH,
				sType:          tFloatHistogram,
			}) {
				continue outer
			}

			t.metrics.enqueueRetriesTotal.Inc()
			time.Sleep(time.Duration(backoff))
			backoff *= 2
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

// Start the queue manager sending samples to the remote storage.
// Does not block.
func (t *QueueManager) Start() {
	// Register and initialise some metrics.
	t.metrics.register()
	t.metrics.shardCapacity.Set(float64(t.cfg.Capacity))
	t.metrics.maxNumShards.Set(float64(t.cfg.MaxShards))
	t.metrics.minNumShards.Set(float64(t.cfg.MinShards))
	t.metrics.desiredNumShards.Set(float64(t.cfg.MinShards))
	t.metrics.maxSamplesPerSend.Set(float64(t.cfg.MaxSamplesPerSend))

	t.shards.start(t.numShards)
	t.watcher.Start()
	if t.mcfg.Send {
		t.metadataWatcher.Start()
	}

	t.wg.Add(2)
	go t.updateShardsLoop()
	go t.reshardLoop()
}

// Stop stops sending samples to the remote storage and waits for pending
// sends to complete.
func (t *QueueManager) Stop() {
	level.Info(t.logger).Log("msg", "Stopping remote storage...")
	defer level.Info(t.logger).Log("msg", "Remote storage stopped.")

	close(t.quit)
	t.wg.Wait()
	// Wait for all QueueManager routines to end before stopping shards, metadata watcher, and WAL watcher. This
	// is to ensure we don't end up executing a reshard and shards.stop() at the same time, which
	// causes a closed channel panic.
	t.shards.stop()
	t.watcher.Stop()
	if t.mcfg.Send {
		t.metadataWatcher.Stop()
	}

	// On shutdown, release the strings in the labels from the intern pool.
	t.seriesMtx.Lock()
	for _, labels := range t.seriesLabels {
		t.releaseLabels(labels)
	}
	t.seriesMtx.Unlock()
	t.metrics.unregister()
}

// StoreSeries keeps track of which series we know about for lookups when sending samples to remote.
func (t *QueueManager) StoreSeries(series []record.RefSeries, index int) {
	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	t.seriesSegmentMtx.Lock()
	defer t.seriesSegmentMtx.Unlock()
	for _, s := range series {
		// Just make sure all the Refs of Series will insert into seriesSegmentIndexes map for tracking.
		t.seriesSegmentIndexes[s.Ref] = index

		t.builder.Reset(s.Labels)
		processExternalLabels(t.builder, t.externalLabels)
		keep := relabel.ProcessBuilder(t.builder, t.relabelConfigs...)
		if !keep {
			t.droppedSeries[s.Ref] = struct{}{}
			continue
		}
		lbls := t.builder.Labels()
		t.internLabels(lbls)

		// We should not ever be replacing a series labels in the map, but just
		// in case we do we need to ensure we do not leak the replaced interned
		// strings.
		if orig, ok := t.seriesLabels[s.Ref]; ok {
			t.releaseLabels(orig)
		}
		t.seriesLabels[s.Ref] = lbls
	}
}

// StoreMetadata keeps track of known series' metadata for lookups when sending samples to remote.
func (t *QueueManager) StoreMetadata(meta []record.RefMetadata) {
	if t.rwFormat < Version2 {
		return
	}

	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	for _, m := range meta {
		t.seriesMetadata[m.Ref] = &metadata.Metadata{
			Type: record.ToMetricType(m.Type),
			Unit: m.Unit,
			Help: m.Help,
		}
	}
}

// UpdateSeriesSegment updates the segment number held against the series,
// so we can trim older ones in SeriesReset.
func (t *QueueManager) UpdateSeriesSegment(series []record.RefSeries, index int) {
	t.seriesSegmentMtx.Lock()
	defer t.seriesSegmentMtx.Unlock()
	for _, s := range series {
		t.seriesSegmentIndexes[s.Ref] = index
	}
}

// SeriesReset is used when reading a checkpoint. WAL Watcher should have
// stored series records with the checkpoints index number, so we can now
// delete any ref ID's lower than that # from the two maps.
func (t *QueueManager) SeriesReset(index int) {
	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	t.seriesSegmentMtx.Lock()
	defer t.seriesSegmentMtx.Unlock()
	// Check for series that are in segments older than the checkpoint
	// that were not also present in the checkpoint.
	for k, v := range t.seriesSegmentIndexes {
		if v < index {
			delete(t.seriesSegmentIndexes, k)
			t.releaseLabels(t.seriesLabels[k])
			delete(t.seriesLabels, k)
			delete(t.seriesMetadata, k)
			delete(t.droppedSeries, k)
		}
	}
}

// SetClient updates the client used by a queue. Used when only client specific
// fields are updated to avoid restarting the queue.
func (t *QueueManager) SetClient(c WriteClient) {
	t.clientMtx.Lock()
	t.storeClient = c
	t.clientMtx.Unlock()
}

func (t *QueueManager) client() WriteClient {
	t.clientMtx.RLock()
	defer t.clientMtx.RUnlock()
	return t.storeClient
}

func (t *QueueManager) internLabels(lbls labels.Labels) {
	lbls.InternStrings(t.interner.intern)
}

func (t *QueueManager) releaseLabels(ls labels.Labels) {
	ls.ReleaseStrings(t.interner.release)
}

// processExternalLabels merges externalLabels into b. If b contains
// a label in externalLabels, the value in b wins.
func processExternalLabels(b *labels.Builder, externalLabels []labels.Label) {
	for _, el := range externalLabels {
		if b.Get(el.Name) == "" {
			b.Set(el.Name, el.Value)
		}
	}
}

func (t *QueueManager) updateShardsLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(shardUpdateDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			desiredShards := t.calculateDesiredShards()
			if !t.shouldReshard(desiredShards) {
				continue
			}
			// Resharding can take some time, and we want this loop
			// to stay close to shardUpdateDuration.
			select {
			case t.reshardChan <- desiredShards:
				level.Info(t.logger).Log("msg", "Remote storage resharding", "from", t.numShards, "to", desiredShards)
				t.numShards = desiredShards
			default:
				level.Info(t.logger).Log("msg", "Currently resharding, skipping.")
			}
		case <-t.quit:
			return
		}
	}
}

// shouldReshard returns whether resharding should occur.
func (t *QueueManager) shouldReshard(desiredShards int) bool {
	if desiredShards == t.numShards {
		return false
	}
	// We shouldn't reshard if Prometheus hasn't been able to send to the
	// remote endpoint successfully within some period of time.
	minSendTimestamp := time.Now().Add(-2 * time.Duration(t.cfg.BatchSendDeadline)).Unix()
	lsts := t.lastSendTimestamp.Load()
	if lsts < minSendTimestamp {
		level.Warn(t.logger).Log("msg", "Skipping resharding, last successful send was beyond threshold", "lastSendTimestamp", lsts, "minSendTimestamp", minSendTimestamp)
		return false
	}
	return true
}

// calculateDesiredShards returns the number of desired shards, which will be
// the current QueueManager.numShards if resharding should not occur for reasons
// outlined in this functions implementation. It is up to the caller to reshard, or not,
// based on the return value.
func (t *QueueManager) calculateDesiredShards() int {
	t.dataOut.tick()
	t.dataDropped.tick()
	t.dataOutDuration.tick()

	// We use the number of incoming samples as a prediction of how much work we
	// will need to do next iteration.  We add to this any pending samples
	// (received - send) so we can catch up with any backlog. We use the average
	// outgoing batch latency to work out how many shards we need.
	var (
		dataInRate      = t.dataIn.rate()
		dataOutRate     = t.dataOut.rate()
		dataKeptRatio   = dataOutRate / (t.dataDropped.rate() + dataOutRate)
		dataOutDuration = t.dataOutDuration.rate() / float64(time.Second)
		dataPendingRate = dataInRate*dataKeptRatio - dataOutRate
		highestSent     = t.metrics.highestSentTimestamp.Get()
		highestRecv     = t.highestRecvTimestamp.Get()
		delay           = highestRecv - highestSent
		dataPending     = delay * dataInRate * dataKeptRatio
	)

	if dataOutRate <= 0 {
		return t.numShards
	}

	var (
		// When behind we will try to catch up on 5% of samples per second.
		backlogCatchup = 0.05 * dataPending
		// Calculate Time to send one sample, averaged across all sends done this tick.
		timePerSample = dataOutDuration / dataOutRate
		desiredShards = timePerSample * (dataInRate*dataKeptRatio + backlogCatchup)
	)
	t.metrics.desiredNumShards.Set(desiredShards)
	level.Debug(t.logger).Log("msg", "QueueManager.calculateDesiredShards",
		"dataInRate", dataInRate,
		"dataOutRate", dataOutRate,
		"dataKeptRatio", dataKeptRatio,
		"dataPendingRate", dataPendingRate,
		"dataPending", dataPending,
		"dataOutDuration", dataOutDuration,
		"timePerSample", timePerSample,
		"desiredShards", desiredShards,
		"highestSent", highestSent,
		"highestRecv", highestRecv,
	)

	// Changes in the number of shards must be greater than shardToleranceFraction.
	var (
		lowerBound = float64(t.numShards) * (1. - shardToleranceFraction)
		upperBound = float64(t.numShards) * (1. + shardToleranceFraction)
	)
	level.Debug(t.logger).Log("msg", "QueueManager.updateShardsLoop",
		"lowerBound", lowerBound, "desiredShards", desiredShards, "upperBound", upperBound)

	desiredShards = math.Ceil(desiredShards) // Round up to be on the safe side.
	if lowerBound <= desiredShards && desiredShards <= upperBound {
		return t.numShards
	}

	numShards := int(desiredShards)
	// Do not downshard if we are more than ten seconds back.
	if numShards < t.numShards && delay > 10.0 {
		level.Debug(t.logger).Log("msg", "Not downsharding due to being too far behind")
		return t.numShards
	}

	switch {
	case numShards > t.cfg.MaxShards:
		numShards = t.cfg.MaxShards
	case numShards < t.cfg.MinShards:
		numShards = t.cfg.MinShards
	}
	return numShards
}

func (t *QueueManager) reshardLoop() {
	defer t.wg.Done()

	for {
		select {
		case numShards := <-t.reshardChan:
			// We start the newShards after we have stopped (the therefore completely
			// flushed) the oldShards, to guarantee we only every deliver samples in
			// order.
			t.shards.stop()
			t.shards.start(numShards)
		case <-t.quit:
			return
		}
	}
}

func (t *QueueManager) newShards() *shards {
	s := &shards{
		qm:   t,
		done: make(chan struct{}),
	}
	return s
}

type shards struct {
	mtx sync.RWMutex // With the WAL, this is never actually contended.

	qm     *QueueManager
	queues []*queue
	// So we can accurately track how many of each are lost during shard shutdowns.
	enqueuedSamples    atomic.Int64
	enqueuedExemplars  atomic.Int64
	enqueuedHistograms atomic.Int64

	// Emulate a wait group with a channel and an atomic int, as you
	// cannot select on a wait group.
	done    chan struct{}
	running atomic.Int32

	// Soft shutdown context will prevent new enqueues and deadlocks.
	softShutdown chan struct{}

	// Hard shutdown context is used to terminate outgoing HTTP connections
	// after giving them a chance to terminate.
	hardShutdown                    context.CancelFunc
	samplesDroppedOnHardShutdown    atomic.Uint32
	exemplarsDroppedOnHardShutdown  atomic.Uint32
	histogramsDroppedOnHardShutdown atomic.Uint32
	metadataDroppedOnHardShutdown   atomic.Uint32
}

// start the shards; must be called before any call to enqueue.
func (s *shards) start(n int) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	s.qm.metrics.pendingSamples.Set(0)
	s.qm.metrics.numShards.Set(float64(n))

	newQueues := make([]*queue, n)
	for i := 0; i < n; i++ {
		newQueues[i] = newQueue(s.qm.cfg.MaxSamplesPerSend, s.qm.cfg.Capacity)
	}

	s.queues = newQueues

	var hardShutdownCtx context.Context
	hardShutdownCtx, s.hardShutdown = context.WithCancel(context.Background())
	s.softShutdown = make(chan struct{})
	s.running.Store(int32(n))
	s.done = make(chan struct{})
	s.enqueuedSamples.Store(0)
	s.enqueuedExemplars.Store(0)
	s.enqueuedHistograms.Store(0)
	s.samplesDroppedOnHardShutdown.Store(0)
	s.exemplarsDroppedOnHardShutdown.Store(0)
	s.histogramsDroppedOnHardShutdown.Store(0)
	s.metadataDroppedOnHardShutdown.Store(0)
	for i := 0; i < n; i++ {
		go s.runShard(hardShutdownCtx, i, newQueues[i])
	}
}

// stop the shards; subsequent call to enqueue will return false.
func (s *shards) stop() {
	// Attempt a clean shutdown, but only wait flushDeadline for all the shards
	// to cleanly exit. As we're doing RPCs, enqueue can block indefinitely.
	// We must be able so call stop concurrently, hence we can only take the
	// RLock here.
	s.mtx.RLock()
	close(s.softShutdown)
	s.mtx.RUnlock()

	// Enqueue should now be unblocked, so we can take the write lock.  This
	// also ensures we don't race with writes to the queues, and get a panic:
	// send on closed channel.
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, queue := range s.queues {
		go queue.FlushAndShutdown(s.done)
	}
	select {
	case <-s.done:
		return
	case <-time.After(s.qm.flushDeadline):
	}

	// Force an unclean shutdown.
	s.hardShutdown()
	<-s.done
	if dropped := s.samplesDroppedOnHardShutdown.Load(); dropped > 0 {
		level.Error(s.qm.logger).Log("msg", "Failed to flush all samples on shutdown", "count", dropped)
	}
	if dropped := s.exemplarsDroppedOnHardShutdown.Load(); dropped > 0 {
		level.Error(s.qm.logger).Log("msg", "Failed to flush all exemplars on shutdown", "count", dropped)
	}
}

// enqueue data (sample or exemplar). If the shard is full, shutting down, or
// resharding, it will return false; in this case, you should back off and
// retry. A shard is full when its configured capacity has been reached,
// specifically, when s.queues[shard] has filled its batchQueue channel and the
// partial batch has also been filled.
func (s *shards) enqueue(ref chunks.HeadSeriesRef, data timeSeries) bool {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	shard := uint64(ref) % uint64(len(s.queues))
	select {
	case <-s.softShutdown:
		return false
	default:
		appended := s.queues[shard].Append(data)
		if !appended {
			return false
		}
		switch data.sType {
		case tSample:
			s.qm.metrics.pendingSamples.Inc()
			s.enqueuedSamples.Inc()
		case tExemplar:
			s.qm.metrics.pendingExemplars.Inc()
			s.enqueuedExemplars.Inc()
		case tHistogram, tFloatHistogram:
			s.qm.metrics.pendingHistograms.Inc()
			s.enqueuedHistograms.Inc()
		}
		return true
	}
}

type queue struct {
	// batchMtx covers operations appending to or publishing the partial batch.
	batchMtx   sync.Mutex
	batch      []timeSeries
	batchQueue chan []timeSeries

	// Since we know there are a limited number of batches out, using a stack
	// is easy and safe so a sync.Pool is not necessary.
	// poolMtx covers adding and removing batches from the batchPool.
	poolMtx   sync.Mutex
	batchPool [][]timeSeries
}

type timeSeries struct {
	seriesLabels   labels.Labels
	value          float64
	histogram      *histogram.Histogram
	floatHistogram *histogram.FloatHistogram
	metadata       *metadata.Metadata
	timestamp      int64
	exemplarLabels labels.Labels
	// The type of series: sample, exemplar, or histogram.
	sType seriesType
}

type seriesType int

const (
	tSample seriesType = iota
	tExemplar
	tHistogram
	tFloatHistogram
	tMetadata
)

func newQueue(batchSize, capacity int) *queue {
	batches := capacity / batchSize
	// Always create an unbuffered channel even if capacity is configured to be
	// less than max_samples_per_send.
	if batches == 0 {
		batches = 1
	}
	return &queue{
		batch:      make([]timeSeries, 0, batchSize),
		batchQueue: make(chan []timeSeries, batches),
		// batchPool should have capacity for everything in the channel + 1 for
		// the batch being processed.
		batchPool: make([][]timeSeries, 0, batches+1),
	}
}

// Append the timeSeries to the buffered batch. Returns false if it
// cannot be added and must be retried.
func (q *queue) Append(datum timeSeries) bool {
	q.batchMtx.Lock()
	defer q.batchMtx.Unlock()
	// TODO: check if metadata now means we've reduced the total # of samples
	// we can batch together here, and if so find a way to not include metadata
	// in the batch size calculation.
	q.batch = append(q.batch, datum)
	if len(q.batch) == cap(q.batch) {
		select {
		case q.batchQueue <- q.batch:
			q.batch = q.newBatch(cap(q.batch))
			return true
		default:
			// Remove the sample we just appended. It will get retried.
			q.batch = q.batch[:len(q.batch)-1]
			return false
		}
	}
	return true
}

func (q *queue) Chan() <-chan []timeSeries {
	return q.batchQueue
}

// Batch returns the current batch and allocates a new batch.
func (q *queue) Batch() []timeSeries {
	q.batchMtx.Lock()
	defer q.batchMtx.Unlock()
	select {
	case batch := <-q.batchQueue:
		return batch
	default:
		batch := q.batch
		q.batch = q.newBatch(cap(batch))
		return batch
	}
}

// ReturnForReuse adds the batch buffer back to the internal pool.
func (q *queue) ReturnForReuse(batch []timeSeries) {
	q.poolMtx.Lock()
	defer q.poolMtx.Unlock()
	if len(q.batchPool) < cap(q.batchPool) {
		q.batchPool = append(q.batchPool, batch[:0])
	}
}

// FlushAndShutdown stops the queue and flushes any samples. No appends can be
// made after this is called.
func (q *queue) FlushAndShutdown(done <-chan struct{}) {
	for q.tryEnqueueingBatch(done) {
		time.Sleep(time.Second)
	}
	q.batch = nil
	close(q.batchQueue)
}

// tryEnqueueingBatch tries to send a batch if necessary. If sending needs to
// be retried it will return true.
func (q *queue) tryEnqueueingBatch(done <-chan struct{}) bool {
	q.batchMtx.Lock()
	defer q.batchMtx.Unlock()
	if len(q.batch) == 0 {
		return false
	}

	select {
	case q.batchQueue <- q.batch:
		return false
	case <-done:
		// The shard has been hard shut down, so no more samples can be sent.
		// No need to try again as we will drop everything left in the queue.
		return false
	default:
		// The batchQueue is full, so we need to try again later.
		return true
	}
}

func (q *queue) newBatch(capacity int) []timeSeries {
	q.poolMtx.Lock()
	defer q.poolMtx.Unlock()
	batches := len(q.batchPool)
	if batches > 0 {
		batch := q.batchPool[batches-1]
		q.batchPool = q.batchPool[:batches-1]
		return batch
	}
	return make([]timeSeries, 0, capacity)
}

func negotiateRWProto(rwFormat config.RemoteWriteFormat, lastHeaderSeen string) (string, config.RemoteWriteFormat) {
	if rwFormat == Version1 {
		// If we're only handling Version1 then all we can do is that with snappy compression.
		return "snappy", Version1
	}
	if rwFormat != Version2 {
		// If we get here then someone has added a new RemoteWriteFormat value but hasn't
		// fixed this function to handle it. Panic!
		panic(fmt.Sprintf("Unhandled RemoteWriteFormat %q", rwFormat))
	}
	if lastHeaderSeen == "" {
		// We haven't had a valid header, so we just default to "0.1.0/snappy".
		return "snappy", Version1
	}
	// We can currently handle:
	// "2.0;snappy"
	// "0.1.0" - implicit compression of snappy
	// lastHeaderSeen should contain a list of tuples.
	// If we find a match to something we can handle then we can return that.
	for _, tuple := range strings.Split(lastHeaderSeen, ",") {
		// Remove spaces from the tuple.
		curr := strings.ReplaceAll(tuple, " ", "")
		switch curr {
		case "2.0;snappy":
			return "snappy", Version2
		case "0.1.0":
			return "snappy", Version1
		}
	}

	// Otherwise we have to default to "0.1.0".
	return "snappy", Version1
}

func (s *shards) runShard(ctx context.Context, shardID int, queue *queue) {
	defer func() {
		if s.running.Dec() == 0 {
			close(s.done)
		}
	}()

	shardNum := strconv.Itoa(shardID)
	symbolTable := newRwSymbolTable()

	// Send batches of at most MaxSamplesPerSend samples to the remote storage.
	// If we have fewer samples than that, flush them out after a deadline anyways.
	var (
		max = s.qm.cfg.MaxSamplesPerSend

		pBuf    = proto.NewBuffer(nil)
		pBufRaw []byte
		buf     []byte
	)
	// TODO(@tpaschalis) Should we also raise the max if we have WAL metadata?
	if s.qm.sendExemplars {
		max += int(float64(max) * 0.1)
	}

	// TODO we should make an interface for the timeseries type
	batchQueue := queue.Chan()
	pendingData := make([]prompb.TimeSeries, max)
	for i := range pendingData {
		pendingData[i].Samples = []prompb.Sample{{}}
		if s.qm.sendExemplars {
			pendingData[i].Exemplars = []prompb.Exemplar{{}}
		}
	}

	pendingMinStrData := make([]writev2.TimeSeries, max)
	for i := range pendingMinStrData {
		pendingMinStrData[i].Samples = []writev2.Sample{{}}
	}

	timer := time.NewTimer(time.Duration(s.qm.cfg.BatchSendDeadline))
	stop := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stop()

	attemptBatchSend := func(batch []timeSeries, rwFormat config.RemoteWriteFormat, compression string, timer bool) error {
		switch rwFormat {
		case Version1:
			nPendingSamples, nPendingExemplars, nPendingHistograms := populateTimeSeries(batch, pendingData, s.qm.sendExemplars, s.qm.sendNativeHistograms)
			n := nPendingSamples + nPendingExemplars + nPendingHistograms
			if timer {
				level.Debug(s.qm.logger).Log("msg", "runShard timer ticked, sending buffered data", "samples", nPendingSamples,
					"exemplars", nPendingExemplars, "shard", shardNum, "histograms", nPendingHistograms)
			}
			return s.sendSamples(ctx, pendingData[:n], nPendingSamples, nPendingExemplars, nPendingHistograms, pBuf, &buf, compression)
		case Version2:
			nPendingSamples, nPendingExemplars, nPendingHistograms, nPendingMetadata := populateV2TimeSeries(&symbolTable, batch, pendingMinStrData, s.qm.sendExemplars, s.qm.sendNativeHistograms)
			n := nPendingSamples + nPendingExemplars + nPendingHistograms
			err := s.sendV2Samples(ctx, pendingMinStrData[:n], symbolTable.LabelsStrings(), nPendingSamples, nPendingExemplars, nPendingHistograms, nPendingMetadata, &pBufRaw, &buf, compression)
			symbolTable.clear()
			return err
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			// In this case we drop all samples in the buffer and the queue.
			// Remove them from pending and mark them as failed.
			droppedSamples := int(s.enqueuedSamples.Load())
			droppedExemplars := int(s.enqueuedExemplars.Load())
			droppedHistograms := int(s.enqueuedHistograms.Load())
			s.qm.metrics.pendingSamples.Sub(float64(droppedSamples))
			s.qm.metrics.pendingExemplars.Sub(float64(droppedExemplars))
			s.qm.metrics.pendingHistograms.Sub(float64(droppedHistograms))
			s.qm.metrics.failedSamplesTotal.Add(float64(droppedSamples))
			s.qm.metrics.failedExemplarsTotal.Add(float64(droppedExemplars))
			s.qm.metrics.failedHistogramsTotal.Add(float64(droppedHistograms))
			s.samplesDroppedOnHardShutdown.Add(uint32(droppedSamples))
			s.exemplarsDroppedOnHardShutdown.Add(uint32(droppedExemplars))
			s.histogramsDroppedOnHardShutdown.Add(uint32(droppedHistograms))
			return

		case batch, ok := <-batchQueue:
			if !ok {
				return
			}
			// Work out what version to send based on the last header seen and the QM's rwFormat setting.
			for attemptNos := 1; attemptNos <= 3; attemptNos++ {
				lastHeaderSeen := s.qm.storeClient.GetLastRWHeader()
				compression, rwFormat := negotiateRWProto(s.qm.rwFormat, lastHeaderSeen)
				sendErr := attemptBatchSend(batch, rwFormat, compression, false)
				pErr := &ErrRenegotiate{}
				if sendErr == nil || !errors.As(sendErr, &pErr) {
					// No error, or error wasn't a 406 or 400, so we can stop trying.
					break
				}
				// If we get either of the two errors (406, 400) bundled in ErrRenegotiate we loop and re-negotiate.
				// TODO(alexg) - add retry/renegotiate metrics here
			}

			queue.ReturnForReuse(batch)

			stop()
			timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))

		case <-timer.C:
			batch := queue.Batch()
			if len(batch) > 0 {
				for attemptNos := 1; attemptNos <= 3; attemptNos++ {
					// Work out what version to send based on the last header seen and the QM's rwFormat setting.
					lastHeaderSeen := s.qm.storeClient.GetLastRWHeader()
					compression, rwFormat := negotiateRWProto(s.qm.rwFormat, lastHeaderSeen)
					sendErr := attemptBatchSend(batch, rwFormat, compression, true)
					pErr := &ErrRenegotiate{}
					if sendErr == nil || !errors.As(sendErr, &pErr) {
						// No error, or error wasn't a 406 or 400, so we can stop trying.
						break
					}
					// If we get either of the two errors (406, 400) bundled in ErrRenegotiate we loop and re-negotiate.
				}
				// TODO(alexg) - add retry/renegotiate metrics here
			}
			queue.ReturnForReuse(batch)
			timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))
		}
	}
}

func populateTimeSeries(batch []timeSeries, pendingData []prompb.TimeSeries, sendExemplars, sendNativeHistograms bool) (int, int, int) {
	var nPendingSamples, nPendingExemplars, nPendingHistograms int
	for nPending, d := range batch {
		pendingData[nPending].Samples = pendingData[nPending].Samples[:0]
		if sendExemplars {
			pendingData[nPending].Exemplars = pendingData[nPending].Exemplars[:0]
		}
		if sendNativeHistograms {
			pendingData[nPending].Histograms = pendingData[nPending].Histograms[:0]
		}

		// Number of pending samples is limited by the fact that sendSamples (via sendSamplesWithBackoff)
		// retries endlessly, so once we reach max samples, if we can never send to the endpoint we'll
		// stop reading from the queue. This makes it safe to reference pendingSamples by index.
		pendingData[nPending].Labels = labelsToLabelsProto(d.seriesLabels, pendingData[nPending].Labels)

		switch d.sType {
		case tSample:
			pendingData[nPending].Samples = append(pendingData[nPending].Samples, prompb.Sample{
				Value:     d.value,
				Timestamp: d.timestamp,
			})
			nPendingSamples++
		case tExemplar:
			pendingData[nPending].Exemplars = append(pendingData[nPending].Exemplars, prompb.Exemplar{
				Labels:    labelsToLabelsProto(d.exemplarLabels, nil),
				Value:     d.value,
				Timestamp: d.timestamp,
			})
			nPendingExemplars++
		case tHistogram:
			pendingData[nPending].Histograms = append(pendingData[nPending].Histograms, HistogramToHistogramProto(d.timestamp, d.histogram))
			nPendingHistograms++
		case tFloatHistogram:
			pendingData[nPending].Histograms = append(pendingData[nPending].Histograms, FloatHistogramToHistogramProto(d.timestamp, d.floatHistogram))
			nPendingHistograms++
		}
	}
	return nPendingSamples, nPendingExemplars, nPendingHistograms
}

func (s *shards) sendSamples(ctx context.Context, samples []prompb.TimeSeries, sampleCount, exemplarCount, histogramCount int, pBuf *proto.Buffer, buf *[]byte, compression string) error {
	begin := time.Now()
	err := s.sendSamplesWithBackoff(ctx, samples, sampleCount, exemplarCount, histogramCount, 0, pBuf, buf, compression)
	s.updateMetrics(ctx, err, sampleCount, exemplarCount, histogramCount, 0, time.Since(begin))

	// Return the error in case it is a 406 and we need to reformat the data.
	return err
}

func (s *shards) sendV2Samples(ctx context.Context, samples []writev2.TimeSeries, labels []string, sampleCount, exemplarCount, histogramCount, metadataCount int, pBuf, buf *[]byte, compression string) error {
	begin := time.Now()
	err := s.sendV2SamplesWithBackoff(ctx, samples, labels, sampleCount, exemplarCount, histogramCount, metadataCount, pBuf, buf, compression)
	s.updateMetrics(ctx, err, sampleCount, exemplarCount, histogramCount, metadataCount, time.Since(begin))

	// Return the error in case it is a 406 and we need to reformat the data.
	return err
}

func (s *shards) updateMetrics(ctx context.Context, err error, sampleCount, exemplarCount, histogramCount, metadataCount int, duration time.Duration) {
	if err != nil {
		level.Error(s.qm.logger).Log("msg", "non-recoverable error", "count", sampleCount, "exemplarCount", exemplarCount, "err", err)
		s.qm.metrics.failedSamplesTotal.Add(float64(sampleCount))
		s.qm.metrics.failedExemplarsTotal.Add(float64(exemplarCount))
		s.qm.metrics.failedHistogramsTotal.Add(float64(histogramCount))
	}

	// These counters are used to calculate the dynamic sharding, and as such
	// should be maintained irrespective of success or failure.
	s.qm.dataOut.incr(int64(sampleCount + exemplarCount + histogramCount + metadataCount))
	s.qm.dataOutDuration.incr(int64(duration))
	s.qm.lastSendTimestamp.Store(time.Now().Unix())
	// Pending samples/exemplars/histograms also should be subtracted, as an error means
	// they will not be retried.
	s.qm.metrics.pendingSamples.Sub(float64(sampleCount))
	s.qm.metrics.pendingExemplars.Sub(float64(exemplarCount))
	s.qm.metrics.pendingHistograms.Sub(float64(histogramCount))
	s.enqueuedSamples.Sub(int64(sampleCount))
	s.enqueuedExemplars.Sub(int64(exemplarCount))
	s.enqueuedHistograms.Sub(int64(histogramCount))
}

// sendSamples to the remote storage with backoff for recoverable errors.
func (s *shards) sendSamplesWithBackoff(ctx context.Context, samples []prompb.TimeSeries, sampleCount, exemplarCount, histogramCount, metadataCount int, pBuf *proto.Buffer, buf *[]byte, compression string) error {
	// Build the WriteRequest with no metadata.
	req, highest, lowest, err := buildWriteRequest(s.qm.logger, samples, nil, pBuf, buf, nil, compression)
	s.qm.buildRequestLimitTimestamp.Store(lowest)
	if err != nil {
		// Failing to build the write request is non-recoverable, since it will
		// only error if marshaling the proto to bytes fails.
		return err
	}

	reqSize := len(req)
	*buf = req

	// An anonymous function allows us to defer the completion of our per-try spans
	// without causing a memory leak, and it has the nice effect of not propagating any
	// parameters for sendSamplesWithBackoff/3.
	attemptStore := func(try int) error {
		currentTime := time.Now()
		lowest := s.qm.buildRequestLimitTimestamp.Load()
		if isSampleOld(currentTime, time.Duration(s.qm.cfg.SampleAgeLimit), lowest) {
			// This will filter out old samples during retries.
			req, _, lowest, err := buildWriteRequest(
				s.qm.logger,
				samples,
				nil,
				pBuf,
				buf,
				isTimeSeriesOldFilter(s.qm.metrics, currentTime, time.Duration(s.qm.cfg.SampleAgeLimit)),
				compression,
			)
			s.qm.buildRequestLimitTimestamp.Store(lowest)
			if err != nil {
				return err
			}
			*buf = req
		}

		ctx, span := otel.Tracer("").Start(ctx, "Remote Send Batch")
		defer span.End()

		span.SetAttributes(
			attribute.Int("request_size", reqSize),
			attribute.Int("samples", sampleCount),
			attribute.Int("try", try),
			attribute.String("remote_name", s.qm.storeClient.Name()),
			attribute.String("remote_url", s.qm.storeClient.Endpoint()),
		)

		if exemplarCount > 0 {
			span.SetAttributes(attribute.Int("exemplars", exemplarCount))
		}
		if histogramCount > 0 {
			span.SetAttributes(attribute.Int("histograms", histogramCount))
		}

		begin := time.Now()
		s.qm.metrics.samplesTotal.Add(float64(sampleCount))
		s.qm.metrics.exemplarsTotal.Add(float64(exemplarCount))
		s.qm.metrics.histogramsTotal.Add(float64(histogramCount))
		s.qm.metrics.metadataTotal.Add(float64(metadataCount))
		err := s.qm.client().Store(ctx, *buf, try, Version1, compression)
		s.qm.metrics.sentBatchDuration.Observe(time.Since(begin).Seconds())

		if err != nil {
			span.RecordError(err)
			return err
		}

		return nil
	}

	onRetry := func() {
		s.qm.metrics.retriedSamplesTotal.Add(float64(sampleCount))
		s.qm.metrics.retriedExemplarsTotal.Add(float64(exemplarCount))
		s.qm.metrics.retriedHistogramsTotal.Add(float64(histogramCount))
	}

	err = sendWriteRequestWithBackoff(ctx, s.qm.cfg, s.qm.logger, attemptStore, onRetry)
	if errors.Is(err, context.Canceled) {
		// When there is resharding, we cancel the context for this queue, which means the data is not sent.
		// So we exit early to not update the metrics.
		return err
	}

	s.qm.metrics.sentBytesTotal.Add(float64(reqSize))
	s.qm.metrics.highestSentTimestamp.Set(float64(highest / 1000))

	return err
}

// sendV2Samples to the remote storage with backoff for recoverable errors.
func (s *shards) sendV2SamplesWithBackoff(ctx context.Context, samples []writev2.TimeSeries, labels []string, sampleCount, exemplarCount, histogramCount, metadataCount int, pBuf, buf *[]byte, compression string) error {
	// Build the WriteRequest with no metadata.
	req, highest, lowest, err := buildV2WriteRequest(s.qm.logger, samples, labels, pBuf, buf, nil, compression)
	s.qm.buildRequestLimitTimestamp.Store(lowest)
	if err != nil {
		// Failing to build the write request is non-recoverable, since it will
		// only error if marshaling the proto to bytes fails.
		return err
	}

	reqSize := len(req)
	*buf = req

	// An anonymous function allows us to defer the completion of our per-try spans
	// without causing a memory leak, and it has the nice effect of not propagating any
	// parameters for sendSamplesWithBackoff/3.
	attemptStore := func(try int) error {
		currentTime := time.Now()
		lowest := s.qm.buildRequestLimitTimestamp.Load()
		if isSampleOld(currentTime, time.Duration(s.qm.cfg.SampleAgeLimit), lowest) {
			// This will filter out old samples during retries.
			req, _, lowest, err := buildV2WriteRequest(
				s.qm.logger,
				samples,
				labels,
				pBuf,
				buf,
				isV2TimeSeriesOldFilter(s.qm.metrics, currentTime, time.Duration(s.qm.cfg.SampleAgeLimit)),
				compression,
			)
			s.qm.buildRequestLimitTimestamp.Store(lowest)
			if err != nil {
				return err
			}
			*buf = req
		}

		ctx, span := otel.Tracer("").Start(ctx, "Remote Send Batch")
		defer span.End()

		span.SetAttributes(
			attribute.Int("request_size", reqSize),
			attribute.Int("samples", sampleCount),
			attribute.Int("try", try),
			attribute.String("remote_name", s.qm.storeClient.Name()),
			attribute.String("remote_url", s.qm.storeClient.Endpoint()),
		)

		if exemplarCount > 0 {
			span.SetAttributes(attribute.Int("exemplars", exemplarCount))
		}
		if histogramCount > 0 {
			span.SetAttributes(attribute.Int("histograms", histogramCount))
		}

		begin := time.Now()
		s.qm.metrics.samplesTotal.Add(float64(sampleCount))
		s.qm.metrics.exemplarsTotal.Add(float64(exemplarCount))
		s.qm.metrics.histogramsTotal.Add(float64(histogramCount))
		s.qm.metrics.metadataTotal.Add(float64(metadataCount))
		err := s.qm.client().Store(ctx, *buf, try, Version2, compression)
		s.qm.metrics.sentBatchDuration.Observe(time.Since(begin).Seconds())

		if err != nil {
			span.RecordError(err)
			return err
		}

		return nil
	}

	onRetry := func() {
		s.qm.metrics.retriedSamplesTotal.Add(float64(sampleCount))
		s.qm.metrics.retriedExemplarsTotal.Add(float64(exemplarCount))
		s.qm.metrics.retriedHistogramsTotal.Add(float64(histogramCount))
	}

	err = sendWriteRequestWithBackoff(ctx, s.qm.cfg, s.qm.logger, attemptStore, onRetry)
	if errors.Is(err, context.Canceled) {
		// When there is resharding, we cancel the context for this queue, which means the data is not sent.
		// So we exit early to not update the metrics.
		return err
	}

	s.qm.metrics.sentBytesTotal.Add(float64(reqSize))
	s.qm.metrics.highestSentTimestamp.Set(float64(highest / 1000))

	return err
}

func populateV2TimeSeries(symbolTable *rwSymbolTable, batch []timeSeries, pendingData []writev2.TimeSeries, sendExemplars, sendNativeHistograms bool) (int, int, int, int) {
	var nPendingSamples, nPendingExemplars, nPendingHistograms, nPendingMetadata int
	for nPending, d := range batch {
		pendingData[nPending].Samples = pendingData[nPending].Samples[:0]
		// todo: should we also safeguard against empty metadata here?
		if d.metadata != nil {
			pendingData[nPending].Metadata.Type = metricTypeToMetricTypeProtoV2(d.metadata.Type)
			pendingData[nPending].Metadata.HelpRef = symbolTable.RefStr(d.metadata.Help)
			pendingData[nPending].Metadata.HelpRef = symbolTable.RefStr(d.metadata.Unit)
			nPendingMetadata++
		}

		if sendExemplars {
			pendingData[nPending].Exemplars = pendingData[nPending].Exemplars[:0]
		}
		if sendNativeHistograms {
			pendingData[nPending].Histograms = pendingData[nPending].Histograms[:0]
		}

		// Number of pending samples is limited by the fact that sendSamples (via sendSamplesWithBackoff)
		// retries endlessly, so once we reach max samples, if we can never send to the endpoint we'll
		// stop reading from the queue. This makes it safe to reference pendingSamples by index.
		// pendingData[nPending].Labels = labelsToLabelsProto(d.seriesLabels, pendingData[nPending].Labels)

		pendingData[nPending].LabelsRefs = labelsToLabelsProtoV2Refs(d.seriesLabels, symbolTable, pendingData[nPending].LabelsRefs)
		switch d.sType {
		case tSample:
			pendingData[nPending].Samples = append(pendingData[nPending].Samples, writev2.Sample{
				Value:     d.value,
				Timestamp: d.timestamp,
			})
			nPendingSamples++
		case tExemplar:
			pendingData[nPending].Exemplars = append(pendingData[nPending].Exemplars, writev2.Exemplar{
				LabelsRefs: labelsToLabelsProtoV2Refs(d.exemplarLabels, symbolTable, nil), // TODO: optimize, reuse slice
				Value:      d.value,
				Timestamp:  d.timestamp,
			})
			nPendingExemplars++
		case tHistogram:
			pendingData[nPending].Histograms = append(pendingData[nPending].Histograms, HistogramToMinHistogramProto(d.timestamp, d.histogram))
			nPendingHistograms++
		case tFloatHistogram:
			pendingData[nPending].Histograms = append(pendingData[nPending].Histograms, FloatHistogramToMinHistogramProto(d.timestamp, d.floatHistogram))
			nPendingHistograms++
		case tMetadata:
			// TODO: log or return an error?
			// we shouldn't receive metadata type data here, it should already be inserted into the timeSeries
		}
	}
	return nPendingSamples, nPendingExemplars, nPendingHistograms, nPendingMetadata
}

func sendWriteRequestWithBackoff(ctx context.Context, cfg config.QueueConfig, l log.Logger, attempt func(int) error, onRetry func()) error {
	backoff := cfg.MinBackoff
	sleepDuration := model.Duration(0)
	try := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := attempt(try)

		if err == nil {
			return nil
		}

		// If the error is unrecoverable, we should not retry.
		var backoffErr RecoverableError
		if !errors.As(err, &backoffErr) {
			return err
		}

		sleepDuration = backoff
		switch {
		case backoffErr.retryAfter > 0:
			sleepDuration = backoffErr.retryAfter
			level.Info(l).Log("msg", "Retrying after duration specified by Retry-After header", "duration", sleepDuration)
		case backoffErr.retryAfter < 0:
			level.Debug(l).Log("msg", "retry-after cannot be in past, retrying using default backoff mechanism")
		}

		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(sleepDuration)):
		}

		// If we make it this far, we've encountered a recoverable error and will retry.
		onRetry()
		level.Warn(l).Log("msg", "Failed to send batch, retrying", "err", err)

		backoff = sleepDuration * 2

		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}

		try++
	}
}

func buildTimeSeries(timeSeries []prompb.TimeSeries, filter func(prompb.TimeSeries) bool) (int64, int64, []prompb.TimeSeries, int, int, int) {
	var highest int64
	var lowest int64
	var droppedSamples, droppedExemplars, droppedHistograms int

	keepIdx := 0
	lowest = math.MaxInt64
	for i, ts := range timeSeries {
		if filter != nil && filter(ts) {
			if len(ts.Samples) > 0 {
				droppedSamples++
			}
			if len(ts.Exemplars) > 0 {
				droppedExemplars++
			}
			if len(ts.Histograms) > 0 {
				droppedHistograms++
			}
			continue
		}

		// At the moment we only ever append a TimeSeries with a single sample or exemplar in it.
		if len(ts.Samples) > 0 && ts.Samples[0].Timestamp > highest {
			highest = ts.Samples[0].Timestamp
		}
		if len(ts.Exemplars) > 0 && ts.Exemplars[0].Timestamp > highest {
			highest = ts.Exemplars[0].Timestamp
		}
		if len(ts.Histograms) > 0 && ts.Histograms[0].Timestamp > highest {
			highest = ts.Histograms[0].Timestamp
		}

		// Get lowest timestamp
		if len(ts.Samples) > 0 && ts.Samples[0].Timestamp < lowest {
			lowest = ts.Samples[0].Timestamp
		}
		if len(ts.Exemplars) > 0 && ts.Exemplars[0].Timestamp < lowest {
			lowest = ts.Exemplars[0].Timestamp
		}
		if len(ts.Histograms) > 0 && ts.Histograms[0].Timestamp < lowest {
			lowest = ts.Histograms[0].Timestamp
		}

		// Move the current element to the write position and increment the write pointer
		timeSeries[keepIdx] = timeSeries[i]
		keepIdx++
	}

	timeSeries = timeSeries[:keepIdx]
	return highest, lowest, timeSeries, droppedSamples, droppedExemplars, droppedHistograms
}

func compressPayload(tmpbuf *[]byte, inp []byte, compression string) ([]byte, error) {
	var compressed []byte

	switch compression {
	case "snappy":
		compressed = snappy.Encode(*tmpbuf, inp)
		if n := snappy.MaxEncodedLen(len(inp)); n > len(*tmpbuf) {
			// grow the buffer for the next time
			*tmpbuf = make([]byte, n)
		}
		return compressed, nil
	default:
		return compressed, fmt.Errorf("Unknown compression scheme [%s]", compression)
	}
}

func buildWriteRequest(logger log.Logger, timeSeries []prompb.TimeSeries, metadata []prompb.MetricMetadata, pBuf *proto.Buffer, buf *[]byte, filter func(prompb.TimeSeries) bool, compression string) ([]byte, int64, int64, error) {
	highest, lowest, timeSeries,
		droppedSamples, droppedExemplars, droppedHistograms := buildTimeSeries(timeSeries, filter)

	if droppedSamples > 0 || droppedExemplars > 0 || droppedHistograms > 0 {
		level.Debug(logger).Log("msg", "dropped data due to their age", "droppedSamples", droppedSamples, "droppedExemplars", droppedExemplars, "droppedHistograms", droppedHistograms)
	}

	req := &prompb.WriteRequest{
		Timeseries: timeSeries,
		Metadata:   metadata,
	}

	if pBuf == nil {
		pBuf = proto.NewBuffer(nil) // For convenience in tests. Not efficient.
	} else {
		pBuf.Reset()
	}
	err := pBuf.Marshal(req)
	if err != nil {
		return nil, highest, lowest, err
	}

	// snappy uses len() to see if it needs to allocate a new slice. Make the
	// buffer as long as possible.
	if buf != nil {
		*buf = (*buf)[0:cap(*buf)]
	} else {
		buf = &[]byte{}
	}

	var compressed []byte

	compressed, err = compressPayload(buf, pBuf.Bytes(), compression)
	if err != nil {
		return nil, highest, lowest, err
	}

	return compressed, highest, lowest, nil
}

type rwSymbolTable struct {
	strings    []string
	symbolsMap map[string]uint32
}

func newRwSymbolTable() rwSymbolTable {
	return rwSymbolTable{
		symbolsMap: make(map[string]uint32),
	}
}

func (r *rwSymbolTable) RefStr(str string) uint32 {
	if ref, ok := r.symbolsMap[str]; ok {
		return ref
	}
	ref := uint32(len(r.strings))
	r.strings = append(r.strings, str)
	r.symbolsMap[str] = ref
	return ref
}

func (r *rwSymbolTable) LabelsStrings() []string {
	return r.strings
}

func (r *rwSymbolTable) clear() {
	r.strings = r.strings[:0]
	for k := range r.symbolsMap {
		delete(r.symbolsMap, k)
	}
}

func buildV2WriteRequest(logger log.Logger, samples []writev2.TimeSeries, labels []string, pBuf, buf *[]byte, filter func(writev2.TimeSeries) bool, compression string) ([]byte, int64, int64, error) {
	highest, lowest, timeSeries, droppedSamples, droppedExemplars, droppedHistograms := buildV2TimeSeries(samples, filter)

	if droppedSamples > 0 || droppedExemplars > 0 || droppedHistograms > 0 {
		level.Debug(logger).Log("msg", "dropped data due to their age", "droppedSamples", droppedSamples, "droppedExemplars", droppedExemplars, "droppedHistograms", droppedHistograms)
	}

	req := &writev2.WriteRequest{
		Symbols:    labels,
		Timeseries: timeSeries,
	}

	if pBuf == nil {
		pBuf = &[]byte{} // For convenience in tests. Not efficient.
	}

	data, err := req.OptimizedMarshal(*pBuf)
	if err != nil {
		return nil, highest, lowest, err
	}
	*pBuf = data

	// snappy uses len() to see if it needs to allocate a new slice. Make the
	// buffer as long as possible.
	if buf != nil {
		*buf = (*buf)[0:cap(*buf)]
	} else {
		buf = &[]byte{}
	}

	var compressed []byte

	compressed, err = compressPayload(buf, data, compression)
	if err != nil {
		return nil, highest, lowest, err
	}

	return compressed, highest, lowest, nil
}

func buildV2TimeSeries(timeSeries []writev2.TimeSeries, filter func(writev2.TimeSeries) bool) (int64, int64, []writev2.TimeSeries, int, int, int) {
	var highest int64
	var lowest int64
	var droppedSamples, droppedExemplars, droppedHistograms int

	keepIdx := 0
	lowest = math.MaxInt64
	for i, ts := range timeSeries {
		if filter != nil && filter(ts) {
			if len(ts.Samples) > 0 {
				droppedSamples++
			}
			if len(ts.Exemplars) > 0 {
				droppedExemplars++
			}
			if len(ts.Histograms) > 0 {
				droppedHistograms++
			}
			continue
		}

		// At the moment we only ever append a TimeSeries with a single sample or exemplar in it.
		if len(ts.Samples) > 0 && ts.Samples[0].Timestamp > highest {
			highest = ts.Samples[0].Timestamp
		}
		if len(ts.Exemplars) > 0 && ts.Exemplars[0].Timestamp > highest {
			highest = ts.Exemplars[0].Timestamp
		}
		if len(ts.Histograms) > 0 && ts.Histograms[0].Timestamp > highest {
			highest = ts.Histograms[0].Timestamp
		}

		// Get lowest timestamp
		if len(ts.Samples) > 0 && ts.Samples[0].Timestamp < lowest {
			lowest = ts.Samples[0].Timestamp
		}
		if len(ts.Exemplars) > 0 && ts.Exemplars[0].Timestamp < lowest {
			lowest = ts.Exemplars[0].Timestamp
		}
		if len(ts.Histograms) > 0 && ts.Histograms[0].Timestamp < lowest {
			lowest = ts.Histograms[0].Timestamp
		}

		// Move the current element to the write position and increment the write pointer
		timeSeries[keepIdx] = timeSeries[i]
		keepIdx++
	}

	timeSeries = timeSeries[:keepIdx]
	return highest, lowest, timeSeries, droppedSamples, droppedExemplars, droppedHistograms
}
