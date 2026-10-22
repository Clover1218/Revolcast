package queue

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"revolcast-backend/internal/biz"
)

var (
	ErrPoolClosed = errors.New("queue: pool closed")
	ErrQueueFull  = errors.New("queue: queue full, task dropped")
)

// Handler 业务处理器：ctx 已携带 TaskTimeout 与父 ctx 取消链。
// 返回 error 仅记录 Warn 日志，不自动重试（重试策略由业务层自行决定）。
type Handler func(ctx context.Context, t Task) error

// Config 队列与 Worker 的参数。
type Config struct {
	Workers        int           // Worker 协程数。建议 ≈ Mongo 连接池上限的 1/2，避免抢连接导致 p99 抖动
	QueueSize      int           // 有界队列容量，务必有界以防 OOM
	EnqueueTimeout time.Duration // 队列满时投递最长等待；>0 超时即丢弃，绝不阻塞 readPump
	TaskTimeout    time.Duration // 单条任务处理上限，防止一个卡死的 handler 占死 Worker
}

// Metrics 进程内可观测指标，下阶段可对接 Prometheus。
type Metrics struct {
	Enqueued atomic.Uint64
	Dropped  atomic.Uint64
	Failed   atomic.Uint64
	Inflight atomic.Int64
}

// Pool 生产者-消费者模型的核心：readPump 是生产者，业务 Handler 是消费者。
type Pool struct {
	cfg     Config
	handler Handler
	logger  *zap.Logger
	metrics Metrics

	ctx    context.Context
	cancel context.CancelFunc

	// mu 保护 closed 与 tasks 的关闭关系：
	// Enqueue 持读锁发送，Close 持写锁关闭 channel，杜绝 "send on closed channel" panic。
	mu     sync.RWMutex
	closed bool
	tasks  chan Task

	wg sync.WaitGroup
}

// NewPool 创建一个 Worker Pool 并立即启动 Workers 个消费者协程。
// parent 用于级联取消（如进程收到 SIGTERM）。Handler 不应为 nil。
func NewPool(parent context.Context, cfg Config, h Handler, lg *zap.Logger) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.EnqueueTimeout <= 0 {
		cfg.EnqueueTimeout = 3 * time.Second // 保底值，避免 Close 因某次慢投递拿不到写锁而永久阻塞
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 5 * time.Second
	}

	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		cfg:     cfg,
		handler: h,
		logger:  lg.With(zap.String("module", "Queue")),
		ctx:     ctx,
		cancel:  cancel,
		tasks:   make(chan Task, cfg.QueueSize),
	}

	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	return p
}

// Enqueue 非阻塞（最多 EnqueueTimeout）投递。返回值：
//   - nil          入队成功
//   - ErrQueueFull 队列满且超时，任务被丢弃（调用方应记录 / 落盘降级）
//   - ErrPoolClosed 池已关闭
func (p *Pool) Enqueue(t Task) error {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrPoolClosed
	}

	timer := time.NewTimer(p.cfg.EnqueueTimeout)
	defer timer.Stop()

	select {
	case p.tasks <- t:
		p.mu.RUnlock()
		p.metrics.Enqueued.Add(1)
		return nil
	case <-timer.C:
		p.mu.RUnlock()
		p.metrics.Dropped.Add(1)
		return ErrQueueFull
	case <-p.ctx.Done():
		p.mu.RUnlock()
		p.metrics.Dropped.Add(1)
		return ErrPoolClosed
	}
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	lg := p.logger.With(zap.Int("worker_id", id))

	for {
		select {
		case t, ok := <-p.tasks:
			if !ok {
				// channel 已关闭且消费完毕，正常退出。
				return
			}
			p.execute(p.ctx, lg, t)
		case <-p.ctx.Done():
			// 父 ctx 被取消但未走 Close（异常退出）：尽力消费剩余任务，最多 5 秒，防止彻底丢数据。
			lg.Warn("pool ctx canceled, draining best-effort")
			p.drain(context.Background(), lg, 5*time.Second)
			return
		}
	}
}

// drain 在异常退出路径下，用独立 ctx 把 channel 中剩余任务消费完（受 timeout 限制）。
func (p *Pool) drain(parent context.Context, lg *zap.Logger, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case t, ok := <-p.tasks:
			if !ok {
				return
			}
			p.execute(parent, lg, t)
		case <-timer.C:
			lg.Warn("drain timeout, remaining tasks discarded",
				zap.Int64("inflight", p.metrics.Inflight.Load()),
				zap.Int("queue_len", len(p.tasks)),
			)
			return
		}
	}
}

func (p *Pool) execute(parent context.Context, lg *zap.Logger, t Task) {
	start := time.Now()
	lg = lg.With(
		zap.String("trace_id", t.TraceID),
		zap.String("client_id", t.ClientID),
		zap.String("msg_type", t.Type),
		zap.Duration("queue_wait", start.Sub(t.ReceivedAt)),
	)

	// 每条任务独立 recover，单条 panic 不能带走整个 Worker。
	defer func() {
		if r := recover(); r != nil {
			p.metrics.Failed.Add(1)
			lg.Error("task panic", zap.Any("panic", r), zap.Stack("stack"))
		}
	}()

	ctx, cancel := context.WithTimeout(parent, p.cfg.TaskTimeout)
	defer cancel()

	p.metrics.Inflight.Add(1)
	defer p.metrics.Inflight.Add(-1)

	if err := p.handler(ctx, t); err != nil {
		p.metrics.Failed.Add(1)
		lg.Warn("task handle failed",
			zap.String("error_code", biz.ResolveErrorMeta(err).WSCode),
			zap.Error(err),
			zap.Duration("cost", time.Since(start)),
		)
		return
	}
	lg.Debug("task handled", zap.Duration("cost", time.Since(start)))
}

// Close 优雅关闭，顺序：
//  1. 置 closed 并 close(tasks)，此后 Enqueue 全部失败；
//  2. 等待 Worker 将 channel 中剩余任务消费完毕（关闭后仍能读出剩余元素）；
//  3. 完成后 cancel 内部 ctx。
//
// 若 ctx 先到期，会强制 cancel 打断 in-flight handler 并返回 ctx.Err()。
// 调用方务必保证：先 StopAccepting（停收新连接 / 关已连 WS）→ 再 Close → 最后关 DB。
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.tasks)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.cancel()
		p.logger.Info("queue pool closed cleanly",
			zap.Uint64("enqueued", p.metrics.Enqueued.Load()),
			zap.Uint64("dropped", p.metrics.Dropped.Load()),
			zap.Uint64("failed", p.metrics.Failed.Load()),
		)
		return nil
	case <-ctx.Done():
		p.cancel()
		p.logger.Error("queue pool close timeout, forced cancel",
			zap.Int64("inflight", p.metrics.Inflight.Load()),
			zap.Int("queue_len", len(p.tasks)),
		)
		return ctx.Err()
	}
}

// Metrics 返回指标快照，便于上报或调试。
func (p *Pool) Metrics() *Metrics { return &p.metrics }
