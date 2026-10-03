package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransferOverlapsInputAndBoundsBuffers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	started := make(chan struct{}, TransferConcurrency)
	release := make(chan struct{})
	var active, peak atomic.Int64
	done := make(chan error, 1)
	go func() {
		_, _, err := Transfer(ctx, r, 1024, func(ctx context.Context, i int, data []byte) error {
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- err
	}()
	go func() { _, err := w.Write(bytes.Repeat([]byte("x"), 8*1024)); w.CloseWithError(err) }()
	for range TransferConcurrency {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("blocks did not transfer before input EOF")
		}
	}
	if peak.Load() != TransferConcurrency {
		t.Fatalf("peak transfers %d", peak.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTransferProviderFailureUnblocksInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	failure := errors.New("provider failed")
	done := make(chan error, 1)
	go func() {
		_, _, err := Transfer(ctx, r, 1024, func(context.Context, int, []byte) error { return failure })
		done <- err
	}()
	if _, err := w.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	// The source remains open and would block forever awaiting more input.
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("provider failure left input blocked")
	}
}
