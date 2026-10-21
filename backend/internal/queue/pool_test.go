package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newTestPool(t *testing.T, cfg Config, h Handler) *Pool {
	t.Helper()
	lg, err := zap.NewDevelopment()
	if err != nil {
		t.Fatal(err)
	}
	return NewPool(context.Background(), cfg, h, lg)
}

// 正常路径：Enqueue 后 Handler 被调用一次。
func TestEnqueueHandlerCalled(t *testing.T) {
	var got int64
	p := newTestPool(t, Config{Workers: 4, QueueSize: 16}, func(_ context.Context, _ Task) error {
		atomic.AddInt64(&got, 1)
		return nil
	})
	defer p.Close(context.Background())

	for i := 0; i < 10; i++ {
		if err := p.Enqueue(Task{TraceID: "x"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&got) < 10 {
		select {
		case <-deadline:
			t.Fatalf("handler not called enough: %d/10", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// 优雅关闭必须排空队列内剩余任务，不丢数据。
func TestCloseDrainsRemaining(t *testing.T) {
	var processed int64
	p := newTestPool(t, Config{Workers: 1, QueueSize: 1000}, func(_ context.Context, _ Task) error {
		atomic.AddInt64(&processed, 1)
		time.Sleep(5 * time.Millisecond) // 让队列填满一些
		return nil
	})

	const n = 200
	for i := 0; i < n; i++ {
		_ = p.Enqueue(Task{TraceID: "x"})
	}

	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := atomic.LoadInt64(&processed); got != n {
		t.Fatalf("drain lost data: processed %d, want %d", got, n)
	}
}

// 并发 Enqueue + Close 不得 panic（核心正确性验证）。
func TestConcurrentEnqueueAndClose(t *testing.T) {
	p := newTestPool(t, Config{Workers: 8, QueueSize: 64}, func(_ context.Context, _ Task) error {
		time.Sleep(time.Millisecond)
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = p.Enqueue(Task{TraceID: "x"}) // 忽略 ErrPoolClosed
			}
		}()
	}
	go func() { _ = p.Close(context.Background()) }()

	wg.Wait()
	// 再补一次 Close 必须幂等、不 panic。
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// 队列满时 Enqueue 超时返回 ErrQueueFull 而非永久阻塞。
func TestEnqueueTimeout(t *testing.T) {
	p := newTestPool(t, Config{Workers: 1, QueueSize: 2, EnqueueTimeout: 50 * time.Millisecond},
		func(_ context.Context, _ Task) error { time.Sleep(50 * time.Millisecond); return nil })

	start := time.Now()
	// 填满队列（2 个在途），第 3 个应快速返回 ErrQueueFull。
	for i := 0; i < 3; i++ {
		_ = p.Enqueue(Task{TraceID: "x"})
	}
	if err := p.Enqueue(Task{TraceID: "x"}); err != ErrQueueFull {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("enqueue blocked too long")
	}
	_ = p.Close(context.Background())
}
