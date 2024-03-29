package jobqueue

import (
	"sync"
	"time"
)

type workerSig struct{}
type workerSocket struct {
	terminate chan workerSig
	suspend   chan workerSig
	resume    chan workerSig
}

// queue in an internal Queue implementation.
type queue struct {
	jobTimeout       time.Duration
	retryDelay       time.Duration
	requeueIfTimeout bool

	succeedHandler SuccessHandler
	dropHandler    DropHandler
	errHandler     ErrHandler
	panicHandler   PanicHandler

	rootWorkers workers
	sync        sync.RWMutex
	timerPool   sync.Pool

	jobq    chan *Job
	workerq chan workerSocket

	suspended bool
	closed    bool
}

// Sync return a channel synchronized with the job queue.
// If the returned queue is closed, then unexpected behaviors like panic can occur.
func (q *queue) Sync() chan<- *Job {
	q.sync.RLock()
	defer q.sync.RUnlock()

	if q.closed {
		// if jobq is closed, ignore new job
		sync := make(chan *Job)
		go func() { defer close(sync); <-sync }()
		return sync
	}
	return q.jobq
}

// Scale adds or removes workers to reach the given value.
func (q *queue) Scale(workers uint) (int, error) {
	q.sync.Lock()
	defer q.sync.Unlock()

	delta := int(workers) - q.NumWorkers()
	if delta > 0 {
		return q.addWorkers(delta)
	}
	return q.removeWorker(-delta)
}

// Close flushes and closes the job queue and stop all workers.
func (q *queue) Close() {
	q.sync.RLock()
	if q.closed {
		q.sync.RUnlock()
		return
	}
	q.sync.RUnlock()

	q.sync.Lock()
	q.closed = true

	close(q.jobq)
	for range q.jobq {
	}

	q.sync.Unlock()
	_, _ = q.Scale(0)
	return
}

// WaitAndClose waits the job queue to be empty before closing all workers.
// If all workers are suspended, this function run like Close.
func (q *queue) WaitAndClose() {
	q.sync.RLock()
	if q.closed {
		q.sync.RUnlock()
		return
	}
	if q.suspended {
		q.sync.RUnlock()
		q.Close()
		return
	}
	q.sync.RUnlock()

	q.sync.Lock()
	q.closed = true
	close(q.jobq)
	q.sync.Unlock()

	for q.JobLoad() > 0 {
		time.Sleep(50 * time.Millisecond)
	}

	_, _ = q.Scale(0)
	return
}

// SuspendWorkers suspends all workers.
func (q *queue) SuspendWorkers() {
	q.sync.Lock()
	defer q.sync.Unlock()

	// ignore if already suspended
	if q.suspended {
		return
	}

	workers := len(q.workerq)
	for i := 0; i < workers; i++ {
		worker := <-q.workerq
		worker.suspend <- workerSig{}
		q.workerq <- worker
	}
	q.suspended = true
}

// ResumeWorkers resumes all workers.
func (q *queue) ResumeWorkers() {
	q.sync.Lock()
	defer q.sync.Unlock()

	// ignore if not suspended
	if !q.suspended {
		return
	}

	workers := len(q.workerq)
	for i := 0; i < workers; i++ {
		worker := <-q.workerq
		worker.resume <- workerSig{}
		q.workerq <- worker
	}
	q.suspended = false
}

// NumWorkers returns the number of worker in worker queue.
func (q *queue) WorkersLimit() int { return cap(q.workerq) }

// NumWorkers returns the number of worker in worker queue.
func (q *queue) NumWorkers() int { return len(q.workerq) }

// JobCapacity returns the number of maximum jobs in job queue.
func (q *queue) JobCapacity() int { return cap(q.jobq) }

// JobLoad returns the number of jobs in job queue.
func (q *queue) JobLoad() int { return len(q.jobq) }

// addWorkers add N workers to the worker queue.
func (q *queue) addWorkers(n int) (int, error) {
	for i := 0; i < n; i++ {
		if len(q.workerq) == cap(q.workerq) {
			return i, newErrMaxWorkerReached()
		}

		workers := q.rootWorkers.copy()
		if err := workers.initialize(); err != nil {
			return i, err
		}

		wkch := workerSocket{
			terminate: make(chan workerSig, 1),
			suspend:   make(chan workerSig, 1),
			resume:    make(chan workerSig, 1),
		}
		go q.do(workers, wkch)
		q.workerq <- wkch
	}

	return n, nil
}

// removeWorker remove N workers from the worker queue.
func (q *queue) removeWorker(workers int) (int, error) {
	for i := 0; i < workers; i++ {
		// can't enter in this condition ... (theoretically)
		if len(q.workerq) == 0 {
			return -i, newErrMinWorkerReached()
		}

		worker := <-q.workerq
		worker.terminate <- workerSig{}
		close(worker.terminate)
		close(worker.suspend)
		close(worker.resume)
	}

	return -workers, nil
}

// workers simplify processes with several workers.
type workers []Worker

// copy make a copy of all workers.
func (ws workers) copy() workers {
	copy := make([]Worker, len(ws))

	for i, w := range ws {
		copy[i] = w.Copy()
	}
	return copy
}

// initialize all workers.
func (ws workers) initialize() error {
	for _, w := range ws {
		if err := w.Initialize(); err != nil {
			return err
		}
	}
	return nil
}

// terminate all workers.
func (ws workers) terminate() {
	for _, w := range ws {
		w.Terminate()
	}
}
