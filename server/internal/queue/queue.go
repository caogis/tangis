// Package queue 封装 NATS JetStream 任务队列（PRD 3.2：NATS JetStream 唯一选型）。
//
// 拓扑约定：
//   - Stream「TANGIS」，subjects「TASKS.*」，携带全部任务生命周期事件；
//   - 持久化 consumer「tangis-worker」（durable），filter subjects TASKS.*；
//   - 任务创建事件发布到「TASKS.created」。
//
// Handler/Worker 只依赖 Publisher 接口，便于单测 mock 与 NATS 不可用时优雅降级。
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// DefaultURL 默认 NATS 地址（本机 docker 映射 14222）。
	DefaultURL = "nats://127.0.0.1:14222"

	// StreamName 任务事件流名称。
	StreamName = "TANGIS"
	// SubjectTaskCreated 任务创建事件 subject。
	SubjectTaskCreated = "TASKS.created"
	// streamSubjects stream 覆盖的 subjects 通配。
	streamSubjects = "TASKS.*"
	// DurableWorker worker 侧持久化 consumer 名称。
	DurableWorker = "tangis-worker"

	// connectTimeout 建连超时：NATS 不在时快速失败走降级，不阻塞建任务。
	connectTimeout = 2 * time.Second
	// ackWait 消费 ack 超时，超时未 ack 消息将重投。
	ackWait = 5 * time.Minute
)

// TaskMessage 任务消息体：API -> 队列 -> Worker 的唯一契约。
// Params 为任务参数（F-04 参数版本化），随消息下发内核，可为 nil。
type TaskMessage struct {
	TaskID       string         `json:"task_id"`
	Type         string         `json:"type"`
	Source       string         `json:"source"`
	Output       string         `json:"output"`
	ManifestPath string         `json:"manifest_path"`
	Params       map[string]any `json:"params,omitempty"`
}

// Publisher 任务事件发布接口，Handler 层依赖它而非具体 NATS 实现。
type Publisher interface {
	// Publish 发布一条任务消息；返回 error 时调用方负责降级。
	Publish(msg TaskMessage) error
	// Close 释放底层连接。
	Close()
}

// Queue 基于 NATS JetStream 的 Publisher 实现。
type Queue struct {
	conn *nats.Conn
	js   jetstream.JetStream
}

// Connect 建立 NATS 连接并确保 Stream/Consumer 存在。
// 地址默认 DefaultURL，可由调用方传入 env NATS_URL 解析结果。
func Connect(url string) (*Queue, error) {
	if url == "" {
		url = DefaultURL
	}
	conn, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("queue: connect %s: %w", url, err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: jetstream context: %w", err)
	}

	// 幂等确保 Stream；已存在但配置不同会返回错误，此处忽略 ErrStreamNameExist 之外场景统一向上抛
	stream, err := js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{streamSubjects},
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: ensure stream %s: %w", StreamName, err)
	}
	_ = stream

	// 幂等确保 durable consumer（默认 AckPolicy 即 AckExplicit，ack 超时后重投）
	if _, err := js.CreateOrUpdateConsumer(context.Background(), StreamName, jetstream.ConsumerConfig{
		Durable:       DurableWorker,
		FilterSubject: streamSubjects,
		AckWait:       ackWait,
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("queue: ensure consumer %s: %w", DurableWorker, err)
	}
	return &Queue{conn: conn, js: js}, nil
}

// Publish 实现 Publisher：序列化 TaskMessage 并发布到 TASKS.created。
// JetStream 异步确认，这里同步等待服务端 ack 以便上层可靠降级。
func (q *Queue) Publish(msg TaskMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal task message: %w", err)
	}
	_, err = q.js.Publish(context.Background(), SubjectTaskCreated, data)
	if err != nil {
		return fmt.Errorf("queue: publish %s: %w", SubjectTaskCreated, err)
	}
	return nil
}

// Subscribe 订阅任务创建事件（durable consumer 推模式），
// 每条消息反序列化后回调 handle；回调返回 nil 则 Ack，否则 Nak 触发重投。
func (q *Queue) Subscribe(handle func(TaskMessage) error) error {
	consumer, err := q.js.Consumer(context.Background(), StreamName, DurableWorker)
	if err != nil {
		return fmt.Errorf("queue: get consumer %s: %w", DurableWorker, err)
	}
	_, err = consumer.Consume(func(msg jetstream.Msg) {
		var tm TaskMessage
		if err := json.Unmarshal(msg.Data(), &tm); err != nil {
			// 毒消息：无法解析的负载直接终止，避免无限重投
			_ = msg.Term()
			return
		}
		if err := handle(tm); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.DoubleAck(context.Background())
	})
	if err != nil {
		return fmt.Errorf("queue: subscribe %s: %w", SubjectTaskCreated, err)
	}
	return nil
}

// Close 实现 Publisher：关闭底层连接。
func (q *Queue) Close() {
	if q.conn != nil && !q.conn.IsClosed() {
		q.conn.Close()
	}
}
