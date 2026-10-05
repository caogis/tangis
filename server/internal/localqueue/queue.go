// Package localqueue 提供桌面单机版的进程内任务队列，替代 NATS JetStream。
//
// 设计取舍：桌面模式是单进程、单用户场景，不需要跨节点的持久化/重放，
// 因此队列退化为「带缓冲 channel + 若干 worker goroutine」：
//   - 语义对齐 queue.Queue：Publish 投递、Subscribe 注册回调、Close 释放；
//   - 任务持久化仍由 task.Store（SQLite）负责，重启后未终结任务可由上层决定是否重跑；
//   - worker 内部已有幂等保护（仅 PENDING 执行），重复投递不会重复切片。
package localqueue

import (
	"errors"
	"sync"

	"tangis/server/internal/queue"
)

// bufferSize 待处理消息缓冲；写满时 Publish 转为异步投递，避免阻塞 HTTP 请求。
const bufferSize = 256

// Queue 进程内队列，实现 queue.Publisher。
type Queue struct {
	mu      sync.Mutex
	ch      chan queue.TaskMessage
	workers int
	closed  bool
	wg      sync.WaitGroup
}

// New 创建指定 worker 数量的本地队列（并发执行任务）。
func New(workers int) *Queue {
	if workers <= 0 {
		workers = 1
	}
	return &Queue{workers: workers}
}

// Publish 实现 queue.Publisher：投递任务消息。
// channel 满时改为后台异步投递，保证建任务接口不被切片耗时阻塞。
func (q *Queue) Publish(msg queue.TaskMessage) error {
	q.mu.Lock()
	ch := q.ch
	closed := q.closed
	q.mu.Unlock()
	if ch == nil || closed {
		return errors.New("localqueue: queue not started (call Subscribe first)")
	}
	select {
	case ch <- msg:
		return nil
	default:
		go func() { ch <- msg }()
		return nil
	}
}

// Subscribe 注册处理回调并启动 worker；语义对齐 NATS 版：
// 回调返回 nil 即 Ack，返回 error 由 worker 侧自行决定是否重试。
func (q *Queue) Subscribe(handle func(queue.TaskMessage) error) error {
	if handle == nil {
		return errors.New("localqueue: nil handler")
	}
	q.mu.Lock()
	if q.ch != nil {
		q.mu.Unlock()
		return errors.New("localqueue: already subscribed")
	}
	q.ch = make(chan queue.TaskMessage, bufferSize)
	ch := q.ch
	q.mu.Unlock()

	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for msg := range ch {
				// 回调内部的 panic 不应拖垮整个队列worker
				func() {
					defer func() { _ = recover() }()
					_ = handle(msg)
				}()
			}
		}()
	}
	return nil
}

// Close 实现 queue.Publisher：停止接收新消息并等待在途任务收尾。
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed || q.ch == nil {
		q.closed = true
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.ch)
	q.mu.Unlock()
	q.wg.Wait()
}
