type SampleQuery struct {
	Since time.Time // zero: from the beginning
	Until time.Time // zero: up to now
}

type SampleSource[T Timestamped] interface {
	Samples(ctx context.Context, query SampleQuery) ([]T, error)
}

type LogFileSource[T Timestamped] struct{ path string }

func NewLogFileSource[T Timestamped](path string) LogFileSource[T]
func (source LogFileSource[T]) Samples(ctx context.Context, query SampleQuery) ([]T, error)
