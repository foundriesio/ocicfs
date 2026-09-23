// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
)

const (
	// sealWorkers is the number of background goroutines enabling fs-verity on
	// staged objects. One: on an SD-card store the enable ioctl waits on the
	// card's small-write latency rather than on CPU, and concurrent enables
	// make it slower (measured on a Raspberry Pi 4, 1500 objects: 2 workers
	// took 21% longer than one, 4 workers took twice as long).
	sealWorkers = 1
	// sealQueueDepth bounds how many staged temps may wait to be sealed. A full
	// queue blocks the producer, pacing it to the sealer instead of piling
	// unsealed temps onto the store; the value only smooths jitter between the
	// two and is not performance-sensitive.
	sealQueueDepth = 64
)

// Sealed resolves to an object's fs-verity digest ("sha256:<hex>") once the
// store has enabled verity on it, blocking until then. It is safe to call more
// than once; every call returns the same result.
type Sealed func() (string, error)

// Resolved returns a Sealed that already holds fsv, for objects sealed inline.
func Resolved(fsv string) Sealed {
	return func() (string, error) { return fsv, nil }
}

// Writer stores objects with the fs-verity enable, measure and commit steps
// running on a background worker, so the goroutine producing the objects is
// not held up by each one's sealing. WriteObject stages the bytes on the
// caller's goroutine and returns as soon as the object is queued; the returned
// Sealed yields its fs-verity digest once the worker has committed it.
//
// A Writer serves one batch: NewWriter starts the worker, Close waits for it.
// WriteObject and WriteExpected may be called from several goroutines at
// once; each stages on its caller's goroutine and queues through the one
// channel. A write after or during Close is a contract violation, not a
// guarded case. The first error the worker hits cancels the writer, and that
// error — not the resulting context cancellation — is what every later
// WriteObject, Sealed and Close reports.
type Writer struct {
	s      *Store
	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan *sealJob
	wg     sync.WaitGroup

	closeOnce sync.Once

	mu  sync.Mutex
	err error // first failure, worker- or context-originated
}

// NewWriter starts a Writer whose writes are bounded by ctx: canceling ctx
// fails every write still pending. The caller must Close it.
func (s *Store) NewWriter(ctx context.Context) *Writer {
	ctx, cancel := context.WithCancel(ctx)
	w := &Writer{
		s:      s,
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(chan *sealJob, sealQueueDepth),
	}
	w.wg.Add(sealWorkers)
	for range sealWorkers {
		go w.run()
	}
	return w
}

// WriteObject stages r's bytes into a store temp on the calling goroutine,
// queues the temp for sealing, and returns its content digest together with a
// Sealed that resolves to its fs-verity digest. The copy runs under the
// writer's context, so a worker failure interrupts it; a cancellation of ctx
// itself is observed when the object is queued. On a collision the worker
// confirms the existing object's measured fs-verity digest matches the staged
// one, as the synchronous path does, and Sealed reports that object's digest.
func (w *Writer) WriteObject(ctx context.Context, r io.Reader) (digest.Digest, Sealed, error) {
	return w.write(ctx, "", r)
}

// WriteExpected is WriteObject for content whose digest is known in advance:
// r's bytes must hash to expect. On a mismatch the staged temp is discarded
// before it is queued, so nothing is sealed or committed.
func (w *Writer) WriteExpected(ctx context.Context, expect digest.Digest, r io.Reader) (Sealed, error) {
	if expect == "" {
		return nil, errors.New("objectstore: WriteExpected needs the expected digest")
	}
	_, sealed, err := w.write(ctx, expect, r)
	return sealed, err
}

// Close stops accepting objects, waits for every queued one to be sealed or
// discarded, and returns the first error the batch hit — a worker failure, or
// the cancellation of the context given to NewWriter. It is idempotent.
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		close(w.jobs)
		w.wg.Wait()
		// A batch whose context was canceled did not complete even if every
		// queued job happened to finish first; record that before the writer
		// cancels its own context below.
		if err := w.ctx.Err(); err != nil {
			w.fail(fmt.Errorf("objectstore: %w", err))
		}
		w.cancel()
	})
	return w.firstErr()
}

// write stages r's bytes, which must hash to expect when it is non-empty, and
// queues the temp for sealing.
func (w *Writer) write(ctx context.Context, expect digest.Digest, r io.Reader) (digest.Digest, Sealed, error) {
	if err := w.ctx.Err(); err != nil {
		return "", nil, w.firstErrOr(fmt.Errorf("objectstore: %w", err))
	}
	tempPath, d, err := w.s.stage(w.ctx, ".object-*", expect, r)
	if err != nil {
		return "", nil, w.firstErrOr(err)
	}
	job := &sealJob{temp: tempPath, d: d, done: make(chan struct{})}
	// The temp is the caller's until the send succeeds and the worker's after;
	// exactly one select arm wins, so exactly one side removes it.
	select {
	case w.jobs <- job:
	case <-w.ctx.Done():
		removeTemp(tempPath)
		return "", nil, w.firstErrOr(fmt.Errorf("objectstore: %w", w.ctx.Err()))
	case <-ctx.Done():
		removeTemp(tempPath)
		return "", nil, w.firstErrOr(fmt.Errorf("objectstore: %w", ctx.Err()))
	}
	return d, w.sealed(job), nil
}

// run is the worker: it seals queued temps in order until the queue is closed.
// A failed seal records the batch's first error and cancels the writer, after
// which every remaining job fails at seal's cancellation check and removes its
// temp; every job's done channel is closed either way.
func (w *Writer) run() {
	defer w.wg.Done()
	for job := range w.jobs {
		job.fsv, job.err = w.s.seal(w.ctx, job.temp, job.d)
		if job.err != nil {
			w.fail(job.err)
		}
		close(job.done)
	}
}

// sealed returns the Sealed for job, resolving to the job's result once the
// worker is done with it.
func (w *Writer) sealed(job *sealJob) Sealed {
	return func() (string, error) {
		<-job.done
		if job.err != nil {
			return "", w.firstErrOr(job.err)
		}
		return job.fsv, nil
	}
}

// sealJob is one staged temp waiting for the worker; done is closed once fsv
// and err are set.
type sealJob struct {
	temp string
	d    digest.Digest
	done chan struct{}
	fsv  string
	err  error
}

// fail records err as the batch's first error if none is recorded yet and
// cancels the writer.
func (w *Writer) fail(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
	w.cancel()
}

func (w *Writer) firstErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// firstErrOr returns the batch's first error when one is recorded, so a
// failure the worker caused is reported by name rather than as the context
// cancellation it triggered, and err otherwise.
func (w *Writer) firstErrOr(err error) error {
	if first := w.firstErr(); first != nil {
		return first
	}
	return err
}
