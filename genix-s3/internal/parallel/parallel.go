// Package parallel runs indexed work with bounded concurrency and stops at the
// first error — errgroup.SetLimit, without the dependency.
package parallel

import (
	"context"
	"sync"
)

// Run calls work(ctx, i) for i in [0, n), at most limit at a time. The first
// error cancels ctx for the rest and is returned.
func Run(ctx context.Context, n, limit int, work func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)

	slots := make(chan struct{}, max(limit, 1))

	for i := range n {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}

		if ctx.Err() != nil {
			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			if err := work(ctx, i); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}

	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	return ctx.Err()
}
