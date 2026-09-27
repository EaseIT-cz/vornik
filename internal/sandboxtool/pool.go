package sandboxtool

import (
	"context"
	"sync"
)

// pool bounds concurrent runs (§7.3). With two or more slots, one is reserved
// for voice (interactive), so a burst of uploads cannot delay a voice reply;
// uploads use the rest and queue. With one slot there is no reservation and
// voice and uploads share it in arrival order: uploads never get zero slots
// (S5-N2).
type pool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	size     int
	inUse    int
	reserved int // slots only voice may take
}

func newPool(size int) *pool {
	if size < 1 {
		size = 1
	}
	p := &pool{size: size}
	if size >= 2 {
		p.reserved = 1
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// acquire blocks until a slot the feature may use is free, or ctx ends.
func (p *pool) acquire(ctx context.Context, f Feature) (release func(), err error) {
	limit := p.size - p.reserved
	if isVoice(f) {
		limit = p.size
	}
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	for p.inUse >= limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.inUse++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.inUse--
			p.cond.Broadcast()
			p.mu.Unlock()
		})
	}, nil
}
