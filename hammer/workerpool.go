package main

import "context"

type worker interface {
	Run(ctx context.Context)
	Kill()
}

func newWorkerPool(factory func() worker) workerPool {
	workers := make([]worker, 0)
	pool := workerPool{
		workers: workers,
		factory: factory,
	}
	return pool
}

// workerPool contains a collection of _running_ workers.
type workerPool struct {
	workers []worker
	factory func() worker
}

func (p *workerPool) Grow(ctx context.Context) {
	w := p.factory()
	p.workers = append(p.workers, w)
	go w.Run(ctx)
}

func (p *workerPool) Shrink(ctx context.Context) {
	if len(p.workers) == 0 {
		return
	}
	w := p.workers[len(p.workers)-1]
	p.workers = p.workers[:len(p.workers)-1]
	w.Kill()
}
