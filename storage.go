package main

import (
	"context"
	"time"
)

// SampleQuery is a range of stored samples: half-open, [Since, Until). A zero
// Since means from the beginning, a zero Until means up to the newest record.
type SampleQuery struct {
	Since time.Time
	Until time.Time
}

// SampleSource reads back stored samples, oldest first. The history handlers
// take one of these rather than a path, so where the samples live is not
// theirs to know.
//
// ctx is unused by the log-file reader but part of the contract: a query that
// outlives the request that asked for it should be abandoned, not finished.
type SampleSource[T Timestamped] interface {
	Samples(ctx context.Context, query SampleQuery) ([]T, error)
}

// LogFileSource reads samples out of a JSON Lines log.
type LogFileSource[T Timestamped] struct {
	path string
}

func NewLogFileSource[T Timestamped](path string) LogFileSource[T] {
	return LogFileSource[T]{path: path}
}

func (source LogFileSource[T]) Samples(_ context.Context, query SampleQuery) ([]T, error) {
	samples, err := LoadSamplesSince[T](source.path, query.Since)
	if err != nil {
		return nil, err
	}
	if query.Until.IsZero() {
		return samples, nil
	}
	// LoadSamplesSince has no upper bound, so cut the range here. Samples are
	// chronological, so the first one at or past Until ends it.
	for index, sample := range samples {
		if !sample.SampleTime().Before(query.Until) {
			return samples[:index], nil
		}
	}
	return samples, nil
}
