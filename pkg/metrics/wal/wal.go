package wal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wal"
	"go.uber.org/atomic"
)

func init() {
	GlobalRefID = atomic.NewUint64(0)
}

// ErrWALClosed is an error returned when a WAL operation can't run because the
// storage has already been closed.
var ErrWALClosed = fmt.Errorf("WAL storage closed")

type storageMetrics struct {
	r prometheus.Registerer

	numActiveSeries        prometheus.Gauge
	numDeletedSeries       prometheus.Gauge
	totalCreatedSeries     prometheus.Counter
	totalRemovedSeries     prometheus.Counter
	totalAppendedSamples   prometheus.Counter
	totalAppendedExemplars prometheus.Counter
}

func newStorageMetrics(r prometheus.Registerer) *storageMetrics {
	m := storageMetrics{r: r}
	m.numActiveSeries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agent_wal_storage_active_series",
		Help: "Current number of active series being tracked by the WAL storage",
	})

	m.numDeletedSeries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agent_wal_storage_deleted_series",
		Help: "Current number of series marked for deletion from memory",
	})

	m.totalCreatedSeries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_wal_storage_created_series_total",
		Help: "Total number of created series appended to the WAL",
	})

	m.totalRemovedSeries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_wal_storage_removed_series_total",
		Help: "Total number of created series removed from the WAL",
	})

	m.totalAppendedSamples = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_wal_samples_appended_total",
		Help: "Total number of samples appended to the WAL",
	})

	m.totalAppendedExemplars = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agent_wal_exemplars_appended_total",
		Help: "Total number of exemplars appended to the WAL",
	})

	if r != nil {
		r.MustRegister(
			m.numActiveSeries,
			m.numDeletedSeries,
			m.totalCreatedSeries,
			m.totalRemovedSeries,
			m.totalAppendedSamples,
			m.totalAppendedExemplars,
		)
	}

	return &m
}

func (m *storageMetrics) Unregister() {
	if m.r == nil {
		return
	}
	cs := []prometheus.Collector{
		m.numActiveSeries,
		m.numDeletedSeries,
		m.totalCreatedSeries,
		m.totalRemovedSeries,
		m.totalAppendedSamples,
		m.totalAppendedExemplars,
	}
	for _, c := range cs {
		m.r.Unregister(c)
	}
}

// GlobalRefID can be used when a singleton is needed to keep all reference ids unique
var GlobalRefID *atomic.Uint64

// Storage implements storage.Storage, and just writes to the WAL.
type Storage struct {
	// Embed Queryable/ChunkQueryable for compatibility, but don't actually implement it.
	storage.Queryable
	storage.ChunkQueryable

	// Operations against the WAL must be protected by a mutex so it doesn't get
	// closed in the middle of an operation. Other operations are concurrency-safe, so we
	// use a RWMutex to allow multiple usages of the WAL at once. If the WAL is closed, all
	// operations that change the WAL must fail.
	walMtx    sync.RWMutex
	walClosed bool

	path   string
	wal    *wal.WAL
	logger log.Logger

	appenderPool sync.Pool
	bufPool      sync.Pool

	series *stripeSeries

	deletedMtx sync.Mutex
	deleted    map[chunks.HeadSeriesRef]int // Deleted series, and what WAL segment they must be kept until.

	metrics *storageMetrics

	ref *atomic.Uint64
}

// NewStorageWithRefIDSource uses a global refid source instead of local ones
func NewStorageWithRefIDSource(logger log.Logger, registerer prometheus.Registerer, path string, ref *atomic.Uint64) (*Storage, error) {
	w, err := wal.NewSize(logger, registerer, SubDirectory(path), wal.DefaultSegmentSize, true)
	if err != nil {
		return nil, err
	}

	storage := &Storage{
		path:    path,
		wal:     w,
		logger:  logger,
		deleted: map[chunks.HeadSeriesRef]int{},
		series:  newStripeSeries(),
		metrics: newStorageMetrics(registerer),
		ref:     ref,
	}

	storage.bufPool.New = func() interface{} {
		b := make([]byte, 0, 1024)
		return b
	}

	storage.appenderPool.New = func() interface{} {
		return &appender{
			w:         storage,
			series:    make([]record.RefSeries, 0, 100),
			samples:   make([]record.RefSample, 0, 100),
			exemplars: make([]record.RefExemplar, 0, 10),
		}
	}

	if err := storage.replayWAL(); err != nil {
		level.Warn(storage.logger).Log("msg", "encountered WAL read error, attempting repair", "err", err)

		var ce *wal.CorruptionErr
		if ok := errors.As(err, &ce); !ok {
			return nil, err
		}
		if err := w.Repair(ce); err != nil {
			return nil, fmt.Errorf("repair corrupted WAL: %w", err)
		}
	}

	return storage, nil
}

// NewStorage makes a new Storage.
func NewStorage(logger log.Logger, registerer prometheus.Registerer, path string) (*Storage, error) {
	return NewStorageWithRefIDSource(logger, registerer, path, atomic.NewUint64(0))
}

func (w *Storage) replayWAL() error {
	w.walMtx.RLock()
	defer w.walMtx.RUnlock()

	if w.walClosed {
		return ErrWALClosed
	}

	level.Info(w.logger).Log("msg", "replaying WAL, this may take a while", "dir", w.wal.Dir())
	dir, startFrom, err := wal.LastCheckpoint(w.wal.Dir())
	if err != nil && err != record.ErrNotFound {
		return fmt.Errorf("find last checkpoint: %w", err)
	}

	if err == nil {
		sr, err := wal.NewSegmentsReader(dir)
		if err != nil {
			return fmt.Errorf("open checkpoint: %w", err)
		}
		defer func() {
			if err := sr.Close(); err != nil {
				level.Warn(w.logger).Log("msg", "error while closing the wal segments reader", "err", err)
			}
		}()

		// A corrupted checkpoint is a hard error for now and requires user
		// intervention. There's likely little data that can be recovered anyway.
		if err := w.loadWAL(wal.NewReader(sr)); err != nil {
			return fmt.Errorf("backfill checkpoint: %w", err)
		}
		startFrom++
		level.Info(w.logger).Log("msg", "WAL checkpoint loaded")
	}

	// Find the last segment.
	_, last, err := wal.Segments(w.wal.Dir())
	if err != nil {
		return fmt.Errorf("finding WAL segments: %w", err)
	}

	// Backfill segments from the most recent checkpoint onwards.
	for i := startFrom; i <= last; i++ {
		s, err := wal.OpenReadSegment(wal.SegmentName(w.wal.Dir(), i))
		if err != nil {
			return fmt.Errorf("open WAL segment %d: %w", i, err)
		}

		sr := wal.NewSegmentBufReader(s)
		err = w.loadWAL(wal.NewReader(sr))
		if err := sr.Close(); err != nil {
			level.Warn(w.logger).Log("msg", "error while closing the wal segments reader", "err", err)
		}
		if err != nil {
			return err
		}
		level.Info(w.logger).Log("msg", "WAL segment loaded", "segment", i, "maxSegment", last)
	}

	return nil
}

func (w *Storage) loadWAL(r *wal.Reader) (err error) {
	var (
		dec record.Decoder
	)

	var (
		decoded    = make(chan interface{}, 10)
		errCh      = make(chan error, 1)
		seriesPool = sync.Pool{
			New: func() interface{} {
				return []record.RefSeries{}
			},
		}
		samplesPool = sync.Pool{
			New: func() interface{} {
				return []record.RefSample{}
			},
		}
	)

	go func() {
		defer close(decoded)
		for r.Next() {
			rec := r.Record()
			switch dec.Type(rec) {
			case record.Series:
				series := seriesPool.Get().([]record.RefSeries)[:0]
				series, err = dec.Series(rec, series)
				if err != nil {
					errCh <- &wal.CorruptionErr{
						Err:     fmt.Errorf("decode series: %w", err),
						Segment: r.Segment(),
						Offset:  r.Offset(),
					}
					return
				}
				decoded <- series
			case record.Samples:
				samples := samplesPool.Get().([]record.RefSample)[:0]
				samples, err = dec.Samples(rec, samples)
				if err != nil {
					errCh <- &wal.CorruptionErr{
						Err:     fmt.Errorf("decode samples: %w", err),
						Segment: r.Segment(),
						Offset:  r.Offset(),
					}
				}
				decoded <- samples
			case record.Tombstones, record.Exemplars:
				// We don't care about decoding tombstones or exemplars
				// TODO: If decide to decode exemplars, we should make sure to prepopulate
				// stripeSeries.exemplars in the next block by using setLatestExemplar.
				continue
			default:
				errCh <- &wal.CorruptionErr{
					Err:     fmt.Errorf("invalid record type %v", dec.Type(rec)),
					Segment: r.Segment(),
					Offset:  r.Offset(),
				}
				return
			}
		}
	}()

	var biggestRef = w.ref.Load()

	for d := range decoded {
		switch v := d.(type) {
		case []record.RefSeries:
			for _, s := range v {
				// If this is a new series, create it in memory without a timestamp.
				// If we read in a sample for it, we'll use the timestamp of the latest
				// sample. Otherwise, the series is stale and will be deleted once
				// the truncation is performed.
				if w.series.getByID(s.Ref) == nil {
					series := &memSeries{ref: s.Ref, lset: s.Labels, lastTs: 0}
					w.series.set(s.Labels.Hash(), series)

					w.metrics.numActiveSeries.Inc()
					w.metrics.totalCreatedSeries.Inc()

					if biggestRef <= uint64(s.Ref) {
						biggestRef = uint64(s.Ref)
					}
				}
			}

			//nolint:staticcheck
			seriesPool.Put(v)
		case []record.RefSample:
			for _, s := range v {
				// Update the lastTs for the series based
				series := w.series.getByID(s.Ref)
				if series == nil {
					level.Warn(w.logger).Log("msg", "found sample referencing non-existing series, skipping")
					continue
				}

				series.Lock()
				if s.T > series.lastTs {
					series.lastTs = s.T
				}
				series.Unlock()
			}

			//nolint:staticcheck
			samplesPool.Put(v)
		default:
			panic(fmt.Errorf("unexpected decoded type: %T", d))
		}
	}

	w.ref.Store(biggestRef)

	select {
	case err := <-errCh:
		return err
	default:
	}

	if r.Err() != nil {
		return fmt.Errorf("read records: %w", r.Err())
	}

	return nil
}

// Directory returns the path where the WAL storage is held.
func (w *Storage) Directory() string {
	return w.path
}

// Appender returns a new appender against the storage.
func (w *Storage) Appender(_ context.Context) storage.Appender {
	return w.appenderPool.Get().(storage.Appender)
}

// StartTime always returns 0, nil. It is implemented for compatibility with
// Prometheus, but is unused in the agent.
func (*Storage) StartTime() (int64, error) {
	return 0, nil
}

// Truncate removes all data from the WAL prior to the timestamp specified by
// mint.
func (w *Storage) Truncate(mint int64) error {
	w.walMtx.RLock()
	defer w.walMtx.RUnlock()

	if w.walClosed {
		return ErrWALClosed
	}

	start := time.Now()

	// Garbage collect series that haven't received an update since mint.
	w.gc(mint)
	level.Info(w.logger).Log("msg", "series GC completed", "duration", time.Since(start))

	first, last, err := wal.Segments(w.wal.Dir())
	if err != nil {
		return fmt.Errorf("get segment range: %w", err)
	}

	// Start a new segment, so low ingestion volume instance don't have more WAL
	// than needed.
	err = w.wal.NextSegment()
	if err != nil {
		return fmt.Errorf("next segment: %w", err)
	}

	last-- // Never consider last segment for checkpoint.
	if last < 0 {
		return nil // no segments yet.
	}

	// The lower two thirds of segments should contain mostly obsolete samples.
	// If we have less than two segments, it's not worth checkpointing yet.
	last = first + (last-first)*2/3
	if last <= first {
		return nil
	}

	keep := func(id chunks.HeadSeriesRef) bool {
		if w.series.getByID(id) != nil {
			return true
		}

		w.deletedMtx.Lock()
		_, ok := w.deleted[id]
		w.deletedMtx.Unlock()
		return ok
	}
	if _, err = wal.Checkpoint(w.logger, w.wal, first, last, keep, mint); err != nil {
		return fmt.Errorf("create checkpoint: %w", err)
	}
	if err := w.wal.Truncate(last + 1); err != nil {
		// If truncating fails, we'll just try again at the next checkpoint.
		// Leftover segments will just be ignored in the future if there's a checkpoint
		// that supersedes them.
		level.Error(w.logger).Log("msg", "truncating segments failed", "err", err)
	}

	// The checkpoint is written and segments before it is truncated, so we no
	// longer need to track deleted series that are before it.
	w.deletedMtx.Lock()
	for ref, segment := range w.deleted {
		if segment < first {
			delete(w.deleted, ref)
			w.metrics.totalRemovedSeries.Inc()
		}
	}
	w.metrics.numDeletedSeries.Set(float64(len(w.deleted)))
	w.deletedMtx.Unlock()

	if err := wal.DeleteCheckpoints(w.wal.Dir(), last); err != nil {
		// Leftover old checkpoints do not cause problems down the line beyond
		// occupying disk space.
		// They will just be ignored since a higher checkpoint exists.
		level.Error(w.logger).Log("msg", "delete old checkpoints", "err", err)
	}

	level.Info(w.logger).Log("msg", "WAL checkpoint complete",
		"first", first, "last", last, "duration", time.Since(start))
	return nil
}

// gc removes data before the minimum timestamp from the head.
func (w *Storage) gc(mint int64) {
	deleted := w.series.gc(mint)
	w.metrics.numActiveSeries.Sub(float64(len(deleted)))

	_, last, _ := wal.Segments(w.wal.Dir())
	w.deletedMtx.Lock()
	defer w.deletedMtx.Unlock()

	// We want to keep series records for any newly deleted series
	// until we've passed the last recorded segment. The WAL will
	// still contain samples records with all of the ref IDs until
	// the segment's samples has been deleted from the checkpoint.
	//
	// If the series weren't kept on startup when the WAL was replied,
	// the samples wouldn't be able to be used since there wouldn't
	// be any labels for that ref ID.
	for ref := range deleted {
		w.deleted[ref] = last
	}

	w.metrics.numDeletedSeries.Set(float64(len(w.deleted)))
}

// WriteStalenessMarkers appends a staleness sample for all active series.
func (w *Storage) WriteStalenessMarkers(remoteTsFunc func() int64) error {
	var lastErr error
	var lastTs int64

	app := w.Appender(context.Background())
	it := w.series.iterator()
	for series := range it.Channel() {
		var (
			ref  = series.ref
			lset = series.lset
		)

		ts := timestamp.FromTime(time.Now())
		_, err := app.Append(storage.SeriesRef(ref), lset, ts, math.Float64frombits(value.StaleNaN))
		if err != nil {
			lastErr = err
		}

		// Remove millisecond precision; the remote write timestamp we get
		// only has second precision.
		lastTs = (ts / 1000) * 1000
	}

	if lastErr == nil {
		if err := app.Commit(); err != nil {
			return fmt.Errorf("failed to commit staleness markers: %w", err)
		}

		// Wait for remote write to write the lastTs, but give up after 1m
		level.Info(w.logger).Log("msg", "waiting for remote write to write staleness markers...")

		stopCh := time.After(1 * time.Minute)
		start := time.Now()

	Outer:
		for {
			select {
			case <-stopCh:
				level.Error(w.logger).Log("msg", "timed out waiting for staleness markers to be written")
				break Outer
			default:
				writtenTs := remoteTsFunc()
				if writtenTs >= lastTs {
					duration := time.Since(start)
					level.Info(w.logger).Log("msg", "remote write wrote staleness markers", "duration", duration)
					break Outer
				}

				level.Info(w.logger).Log("msg", "remote write hasn't written staleness markers yet", "remoteTs", writtenTs, "lastTs", lastTs)

				// Wait a bit before reading again
				time.Sleep(5 * time.Second)
			}
		}
	}

	return lastErr
}

// Close closes the storage and all its underlying resources.
func (w *Storage) Close() error {
	w.walMtx.Lock()
	defer w.walMtx.Unlock()

	if w.walClosed {
		return fmt.Errorf("already closed")
	}
	w.walClosed = true

	if w.metrics != nil {
		w.metrics.Unregister()
	}
	return w.wal.Close()
}

type appender struct {
	w         *Storage
	series    []record.RefSeries
	samples   []record.RefSample
	exemplars []record.RefExemplar
}

func (a *appender) Append(ref storage.SeriesRef, l labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	series := a.w.series.getByID(chunks.HeadSeriesRef(ref))
	if series == nil {
		// Ensure no empty or duplicate labels have gotten through. This mirrors the
		// equivalent validation code in the TSDB's headAppender.
		l = l.WithoutEmpty()
		if len(l) == 0 {
			return 0, fmt.Errorf("empty labelset: %w", tsdb.ErrInvalidSample)
		}

		if lbl, dup := l.HasDuplicateLabelNames(); dup {
			return 0, fmt.Errorf("label name %q is not unique: %w", lbl, tsdb.ErrInvalidSample)
		}

		var created bool
		series, created = a.getOrCreate(l)
		if created {
			a.series = append(a.series, record.RefSeries{
				Ref:    series.ref,
				Labels: l,
			})

			a.w.metrics.numActiveSeries.Inc()
			a.w.metrics.totalCreatedSeries.Inc()
		}
	}

	series.Lock()
	defer series.Unlock()

	// Update last recorded timestamp. Used by Storage.gc to determine if a
	// series is stale.
	series.updateTs(t)

	a.samples = append(a.samples, record.RefSample{
		Ref: series.ref,
		T:   t,
		V:   v,
	})

	a.w.metrics.totalAppendedSamples.Inc()
	return storage.SeriesRef(series.ref), nil
}

func (a *appender) getOrCreate(l labels.Labels) (series *memSeries, created bool) {
	hash := l.Hash()

	series = a.w.series.getByHash(hash, l)
	if series != nil {
		return series, false
	}

	ref := chunks.HeadSeriesRef(a.w.ref.Inc())
	series = &memSeries{ref: ref, lset: l}
	a.w.series.set(l.Hash(), series)
	return series, true
}

func (a *appender) AppendExemplar(ref storage.SeriesRef, _ labels.Labels, e exemplar.Exemplar) (storage.SeriesRef, error) {
	cref := chunks.HeadSeriesRef(ref)
	s := a.w.series.getByID(cref)
	if s == nil {
		return 0, fmt.Errorf("unknown series ref. when trying to add exemplar: %d", cref)
	}

	// Ensure no empty labels have gotten through.
	e.Labels = e.Labels.WithoutEmpty()

	if lbl, dup := e.Labels.HasDuplicateLabelNames(); dup {
		return 0, fmt.Errorf("label name %q is not unique: %w", lbl, tsdb.ErrInvalidExemplar)
	}

	// Exemplar label length does not include chars involved in text rendering such as quotes
	// equals sign, or commas. See definition of const ExemplarMaxLabelLength.
	labelSetLen := 0
	for _, l := range e.Labels {
		labelSetLen += utf8.RuneCountInString(l.Name)
		labelSetLen += utf8.RuneCountInString(l.Value)

		if labelSetLen > exemplar.ExemplarMaxLabelSetLength {
			return 0, storage.ErrExemplarLabelLength
		}
	}

	// Check for duplicate vs last stored exemplar for this series, and discard those.
	// Otherwise, record the current exemplar as the latest.
	// Prometheus returns 0 when encountering duplicates, so we do the same here.
	prevExemplar := a.w.series.getLatestExemplar(cref)
	if prevExemplar != nil && prevExemplar.Equals(e) {
		// Duplicate, don't return an error but don't accept the exemplar.
		return 0, nil
	}
	a.w.series.setLatestExemplar(cref, &e)

	a.exemplars = append(a.exemplars, record.RefExemplar{
		Ref:    cref,
		T:      e.Ts,
		V:      e.Value,
		Labels: e.Labels,
	})

	a.w.metrics.totalAppendedExemplars.Inc()
	return storage.SeriesRef(s.ref), nil
}

// Commit submits the collected samples and purges the batch.
func (a *appender) Commit() error {
	a.w.walMtx.RLock()
	defer a.w.walMtx.RUnlock()

	if a.w.walClosed {
		return ErrWALClosed
	}

	var encoder record.Encoder
	buf := a.w.bufPool.Get().([]byte)

	if len(a.series) > 0 {
		buf = encoder.Series(a.series, buf)
		if err := a.w.wal.Log(buf); err != nil {
			return err
		}
		buf = buf[:0]
	}

	if len(a.samples) > 0 {
		buf = encoder.Samples(a.samples, buf)
		if err := a.w.wal.Log(buf); err != nil {
			return err
		}
		buf = buf[:0]
	}

	if len(a.exemplars) > 0 {
		buf = encoder.Exemplars(a.exemplars, buf)
		if err := a.w.wal.Log(buf); err != nil {
			return err
		}
		buf = buf[:0]
	}

	//nolint:staticcheck
	a.w.bufPool.Put(buf)

	for _, sample := range a.samples {
		series := a.w.series.getByID(sample.Ref)
		if series != nil {
			series.Lock()
			series.pendingCommit = false
			series.Unlock()
		}
	}

	return a.Rollback()
}

func (a *appender) Rollback() error {
	a.series = a.series[:0]
	a.samples = a.samples[:0]
	a.exemplars = a.exemplars[:0]
	a.w.appenderPool.Put(a)
	return nil
}
