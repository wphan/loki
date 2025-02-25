package ingester

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cortexproject/cortex/pkg/util"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/weaveworks/common/httpgrpc"

	"github.com/grafana/loki/pkg/chunkenc"
	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
)

var (
	chunksCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "loki",
		Name:      "ingester_chunks_created_total",
		Help:      "The total number of chunks created in the ingester.",
	})
	samplesPerChunk = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "loki",
		Subsystem: "ingester",
		Name:      "samples_per_chunk",
		Help:      "The number of samples in a chunk.",

		Buckets: prometheus.LinearBuckets(4096, 2048, 6),
	})
	blocksPerChunk = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "loki",
		Subsystem: "ingester",
		Name:      "blocks_per_chunk",
		Help:      "The number of blocks in a chunk.",

		Buckets: prometheus.ExponentialBuckets(5, 2, 6),
	})
)

func init() {
	prometheus.MustRegister(chunksCreatedTotal)
	prometheus.MustRegister(samplesPerChunk)
	prometheus.MustRegister(blocksPerChunk)
}

type stream struct {
	// Newest chunk at chunks[n-1].
	// Not thread-safe; assume accesses to this are locked by caller.
	chunks  []chunkDesc
	fp      model.Fingerprint // possibly remapped fingerprint, used in the streams map
	labels  labels.Labels
	factory func() chunkenc.Chunk

	tailers   map[uint32]*tailer
	tailerMtx sync.RWMutex
}

type chunkDesc struct {
	chunk   chunkenc.Chunk
	closed  bool
	flushed time.Time

	lastUpdated time.Time
}

type entryWithError struct {
	entry *logproto.Entry
	e     error
}

func newStream(fp model.Fingerprint, labels labels.Labels, factory func() chunkenc.Chunk) *stream {
	return &stream{
		fp:      fp,
		labels:  labels,
		factory: factory,
		tailers: map[uint32]*tailer{},
	}
}

// consumeChunk manually adds a chunk to the stream that was received during
// ingester chunk transfer.
func (s *stream) consumeChunk(_ context.Context, chunk *logproto.Chunk) error {
	c, err := chunkenc.NewByteChunk(chunk.Data)
	if err != nil {
		return err
	}

	s.chunks = append(s.chunks, chunkDesc{
		chunk: c,
	})
	chunksCreatedTotal.Inc()
	return nil
}

func (s *stream) Push(_ context.Context, entries []logproto.Entry) error {
	if len(s.chunks) == 0 {
		s.chunks = append(s.chunks, chunkDesc{
			chunk: s.factory(),
		})
		chunksCreatedTotal.Inc()
	}

	storedEntries := []logproto.Entry{}
	failedEntriesWithError := []entryWithError{}

	// Don't fail on the first append error - if samples are sent out of order,
	// we still want to append the later ones.
	for i := range entries {
		chunk := &s.chunks[len(s.chunks)-1]
		if chunk.closed || !chunk.chunk.SpaceFor(&entries[i]) {
			// If the chunk has no more space call Close to make sure anything in the head block is cut and compressed
			err := chunk.chunk.Close()
			if err != nil {
				// This should be an unlikely situation, returning an error up the stack doesn't help much here
				// so instead log this to help debug the issue if it ever arises.
				level.Error(util.Logger).Log("msg", "failed to Close chunk", "err", err)
			}
			chunk.closed = true

			samplesPerChunk.Observe(float64(chunk.chunk.Size()))
			blocksPerChunk.Observe(float64(chunk.chunk.Blocks()))
			chunksCreatedTotal.Inc()

			s.chunks = append(s.chunks, chunkDesc{
				chunk: s.factory(),
			})
			chunk = &s.chunks[len(s.chunks)-1]
		}
		if err := chunk.chunk.Append(&entries[i]); err != nil {
			failedEntriesWithError = append(failedEntriesWithError, entryWithError{&entries[i], err})
		} else {
			// send only stored entries to tailers
			storedEntries = append(storedEntries, entries[i])
		}
		chunk.lastUpdated = time.Now()
	}

	if len(storedEntries) != 0 {
		go func() {
			stream := logproto.Stream{Labels: s.labels.String(), Entries: storedEntries}

			closedTailers := []uint32{}

			s.tailerMtx.RLock()
			for _, tailer := range s.tailers {
				if tailer.isClosed() {
					closedTailers = append(closedTailers, tailer.getID())
					continue
				}
				tailer.send(stream)
			}
			s.tailerMtx.RUnlock()

			if len(closedTailers) != 0 {
				s.tailerMtx.Lock()
				defer s.tailerMtx.Unlock()

				for _, closedTailerID := range closedTailers {
					delete(s.tailers, closedTailerID)
				}
			}
		}()
	}

	if len(failedEntriesWithError) > 0 {
		lastEntryWithErr := failedEntriesWithError[len(failedEntriesWithError)-1]
		if lastEntryWithErr.e == chunkenc.ErrOutOfOrder {
			// return bad http status request response with all failed entries
			buf := bytes.Buffer{}
			streamName := s.labels.String()

			for _, entryWithError := range failedEntriesWithError {
				_, _ = fmt.Fprintf(&buf,
					"entry with timestamp %s ignored, reason: '%s' for stream: %s,\n",
					entryWithError.entry.Timestamp.String(), entryWithError.e.Error(), streamName)
			}

			_, _ = fmt.Fprintf(&buf, "total ignored: %d out of %d", len(failedEntriesWithError), len(entries))

			return httpgrpc.Errorf(http.StatusBadRequest, buf.String())
		}
		return lastEntryWithErr.e
	}

	return nil
}

// Returns an iterator.
func (s *stream) Iterator(from, through time.Time, direction logproto.Direction, filter logql.Filter) (iter.EntryIterator, error) {
	iterators := make([]iter.EntryIterator, 0, len(s.chunks))
	for _, c := range s.chunks {
		itr, err := c.chunk.Iterator(from, through, direction, filter)
		if err != nil {
			return nil, err
		}
		if itr != nil {
			iterators = append(iterators, itr)
		}
	}

	if direction != logproto.FORWARD {
		for left, right := 0, len(iterators)-1; left < right; left, right = left+1, right-1 {
			iterators[left], iterators[right] = iterators[right], iterators[left]
		}
	}

	return iter.NewNonOverlappingIterator(iterators, s.labels.String()), nil
}

func (s *stream) addTailer(t *tailer) {
	s.tailerMtx.Lock()
	defer s.tailerMtx.Unlock()

	s.tailers[t.getID()] = t
}

func (s *stream) matchesTailer(t *tailer) bool {
	metric := util.LabelsToMetric(s.labels)
	return t.isWatchingLabels(metric)
}
