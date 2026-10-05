package worker

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"tangis/server/internal/queue"
	"tangis/server/internal/task"
)

// blockingExecutor 模拟可中断的内核执行器：RunKeyed 阻塞直到 Kill 被调用，
// 用于复现"用户取消 → 杀进程 → 内核返回非零退出码"的真实时序。
type blockingExecutor struct {
	mu      sync.Mutex
	waits   map[string]chan struct{}
	killed  []string
	started chan string
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{waits: map[string]chan struct{}{}, started: make(chan string, 4)}
}

func (b *blockingExecutor) Run(name string, args []string) (string, error) {
	return b.RunKeyed("", name, args)
}

func (b *blockingExecutor) RunKeyed(key, name string, args []string) (string, error) {
	ch := make(chan struct{})
	b.mu.Lock()
	b.waits[key] = ch
	b.mu.Unlock()
	b.started <- key
	<-ch
	// 被杀掉的进程在真实世界里就是这样返回的：非零退出 + 部分输出
	return "kernel interrupted", errors.New("signal: killed")
}

func (b *blockingExecutor) Kill(key string) bool {
	b.mu.Lock()
	ch, ok := b.waits[key]
	if ok {
		delete(b.waits, key)
		b.killed = append(b.killed, key)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	close(ch)
	return true
}

// TestInterruptKeepsCancelledState 中断后的任务不得被自动重试改写状态。
// 这是取消/暂停功能的正确性核心：kill 进程 → 内核非零退出 → 若不加标记
// 会被 retryOrFail 当成"执行失败"重跑一遍，用户看到任务又活了。
func TestInterruptKeepsCancelledState(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	exec := newBlockingExecutor()
	pub := &retryPublisher{}
	w := newTestWorker(t, store, "", exec, nil)
	w.Publisher = pub
	w.ProgressInterval = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- w.Handle(msg) }()

	// 等内核"开始执行"，此时任务已是 RUNNING
	if key := <-exec.started; key != msg.TaskID {
		t.Fatalf("started key=%q, want %q", key, msg.TaskID)
	}

	// 模拟 API 侧：先中断内核，再写 CANCELLED
	if !w.Interrupt(msg.TaskID) {
		t.Fatal("Interrupt should hit the running kernel process")
	}
	cur, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	cur.Status = task.StatusCancelled
	cur.ErrMsg = "cancelled by user"
	if err := store.Update(cur); err != nil {
		t.Fatalf("update task: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("Handle: %v", err)
	}

	final, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if final.Status != task.StatusCancelled {
		t.Errorf("status=%s, want CANCELLED（中断被误判为执行失败）", final.Status)
	}
	if len(pub.published) != 0 {
		t.Errorf("cancelled task requeued %d times, want 0", len(pub.published))
	}
	if final.Attempts != 0 {
		t.Errorf("attempts=%d, want 0", final.Attempts)
	}
}

// cancelDuringRunExecutor 模拟"内核正常返回，但期间用户恰好取消"的时序。
type cancelDuringRunExecutor struct {
	store  task.Store
	taskID string
}

func (e *cancelDuringRunExecutor) Run(name string, args []string) (string, error) {
	cur, err := e.store.Get(e.taskID, "")
	if err == nil {
		cur.Status = task.StatusCancelled
		cur.ErrMsg = "cancelled by user"
		_ = e.store.Update(cur)
	}
	return "kernel done", nil
}

// TestCancelWinsOverSuccess 小任务上 kill 与内核正常结束可能几乎同时发生：
// 成功后回写不得把用户写入的 CANCELLED 覆盖成 SUCCEEDED（端到端实测踩到的竞态）。
func TestCancelWinsOverSuccess(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(t, store, "", &cancelDuringRunExecutor{store: store, taskID: msg.TaskID}, nil)
	w.Publisher = &retryPublisher{}

	if err := w.Handle(msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := store.Get(msg.TaskID, "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != task.StatusCancelled {
		t.Errorf("status=%s, want CANCELLED（用户取消被成功回写覆盖）", got.Status)
	}
	if got.ErrMsg != "cancelled by user" {
		t.Errorf("err_msg=%q, want cancellation message preserved", got.ErrMsg)
	}
}

// TestInterruptWithoutRunningProcess 没有在跑的进程时返回 false（排队中或已结束）。
func TestInterruptWithoutRunningProcess(t *testing.T) {
	w := newTestWorker(t, task.NewMemoryStore(), "", newBlockingExecutor(), nil)
	if w.Interrupt("no-such-task") {
		t.Error("Interrupt should report false when nothing is running")
	}
}

// TestInterruptWithPlainExecutor 普通执行器（不可中断）应安全返回 false 而非 panic。
func TestInterruptWithPlainExecutor(t *testing.T) {
	w := newTestWorker(t, task.NewMemoryStore(), "", &fakeExecutor{}, nil)
	if w.Interrupt("t1") {
		t.Error("plain executor cannot be interrupted, want false")
	}
}

// TestCmdExecutorKillKillsSpawnedProcess 真实执行器：登记进程后 Kill 应能终止它。
func TestCmdExecutorKillKillsSpawnedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 unix sleep 命令")
	}
	exec := NewCmdExecutor()

	done := make(chan error, 1)
	go func() {
		_, err := exec.RunKeyed("k1", "sleep", []string{"30"})
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Kill("k1") {
			select {
			case err := <-done:
				if err == nil {
					t.Error("killed process should return a non-nil error")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("RunKeyed did not return after Kill")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Kill never matched the spawned process")
}

// TestCmdExecutorDeregistersAfterRun 进程结束后登记表应被清理，Kill 不再命中。
func TestCmdExecutorDeregistersAfterRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 unix true 命令")
	}
	exec := NewCmdExecutor()
	if _, err := exec.RunKeyed("k1", "true", []string{}); err != nil {
		t.Fatalf("run no-op command: %v", err)
	}
	if exec.Kill("k1") {
		t.Error("Kill should miss a process that already finished")
	}
}

// coopExecutor 模拟支持 B7 协作式取消的内核执行器：RunKeyed 轮询
// --cancel-file 标志文件，出现即返回 exit 130（真实内核的安全退出语义）；
// 仅 Kill 也会终止进程（兜底路径）。
type coopExecutor struct {
	mu      sync.Mutex
	procs   map[string]*coopProc
	killed  []string
	started chan string
}

type coopProc struct {
	flagFile   string
	done       chan struct{}
	closeOnce  sync.Once
	registered bool
}

func newCoopExecutor() *coopExecutor {
	return &coopExecutor{procs: map[string]*coopProc{}, started: make(chan string, 4)}
}

func (c *coopExecutor) Run(name string, args []string) (string, error) {
	return c.RunKeyed("", name, args)
}

func (c *coopExecutor) RunKeyed(key, name string, args []string) (string, error) {
	var flag string
	for i, a := range args {
		if a == "--cancel-file" && i+1 < len(args) {
			flag = args[i+1]
		}
	}
	p := &coopProc{flagFile: flag, done: make(chan struct{}), registered: true}
	c.mu.Lock()
	c.procs[key] = p
	c.mu.Unlock()
	c.started <- key

	defer func() {
		c.mu.Lock()
		delete(c.procs, key)
		c.mu.Unlock()
	}()
	go func() {
		for {
			c.mu.Lock()
			d := p.done
			c.mu.Unlock()
			select {
			case <-d:
				return
			default:
			}
			if flag != "" {
				if _, err := os.Stat(flag); err == nil {
					p.closeOnce.Do(func() { close(p.done) })
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	<-p.done
	// 协作退出在真实世界即 exit 130；Kill 则是 signal: killed
	c.mu.Lock()
	wasKilled := len(c.killed) > 0 && c.killed[len(c.killed)-1] == key
	c.mu.Unlock()
	if wasKilled {
		return "kernel interrupted", errors.New("signal: killed")
	}
	return "CANCELED: 检测到取消标志文件", errors.New("exit status 130")
}

func (c *coopExecutor) Kill(key string) bool {
	c.mu.Lock()
	p, ok := c.procs[key]
	if ok {
		c.killed = append(c.killed, key)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	p.closeOnce.Do(func() { close(p.done) })
	return true
}

func (c *coopExecutor) Running(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.procs[key] != nil && c.procs[key].registered
}

func (c *coopExecutor) killCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.killed {
		if k == key {
			n++
		}
	}
	return n
}

// TestCooperativeCancelViaFlagFile B7 优雅取消：Interrupt 写标志文件 →
// 内核在分块边界自行退出（exit 130）→ 不触发 Kill；标志文件被清理、
// 中断标记生效（不误重试）。
func TestCooperativeCancelViaFlagFile(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	store := task.NewMemoryStore()
	if err := store.Create(newPendingTask(msg)); err != nil {
		t.Fatal(err)
	}

	exec := newCoopExecutor()
	w := newTestWorker(t, store, "", exec, nil)
	w.CancelGracePeriod = 2 * time.Second

	done := make(chan error, 1)
	go func() { done <- w.Handle(msg) }()
	if key := <-exec.started; key != msg.TaskID {
		t.Fatalf("started key=%q, want %q", key, msg.TaskID)
	}

	if !w.Interrupt(msg.TaskID) {
		t.Fatal("Interrupt should hit the running kernel process")
	}
	if n := exec.killCount(msg.TaskID); n != 0 {
		t.Errorf("cooperative cancel should not Kill, got %d kills", n)
	}
	// 标志文件应被清理（resume/重试不得被误取消）
	if _, err := os.Stat(w.cancelFlagPath(msg.TaskID)); !os.IsNotExist(err) {
		t.Error("cancel flag file should be removed after interrupt")
	}

	if err := <-done; err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !w.takeInterrupted(msg.TaskID) {
		t.Error("interrupted flag should be set for cooperative cancel")
	}
}

// TestBuildArgvInjectsCancelFile --cancel-file 注入：默认 build argv、影像
// argv 均携带标志文件路径；KERNEL_ARGS 透传支持 {cancel_file} 占位符。
func TestBuildArgvInjectsCancelFile(t *testing.T) {
	dir := t.TempDir()
	msg := newTestMsg(dir)
	w := newTestWorker(t, task.NewMemoryStore(), "", &fakeExecutor{}, nil)

	want := w.cancelFlagPath(msg.TaskID)

	build := w.buildArgv(msg)
	if got := lastFlagArg(build); got != want {
		t.Errorf("build argv cancel-file = %q, want %q (argv=%v)", got, want, build)
	}
	image := w.buildArgv(queue.TaskMessage{TaskID: msg.TaskID, Type: "image->tiles", Source: "x.tif", Output: msg.Output})
	if got := lastFlagArg(image); got != want {
		t.Errorf("image argv cancel-file = %q, want %q (argv=%v)", got, want, image)
	}

	w.KernelArgs = []string{"build", "{manifest_path}", "--cancel-file", "{cancel_file}"}
	passthrough := w.buildArgv(msg)
	if got := lastFlagArg(passthrough); got != want {
		t.Errorf("passthrough cancel-file = %q, want %q (argv=%v)", got, want, passthrough)
	}
}

// lastFlagArg 提取 argv 中 --cancel-file 的值（缺失返回空串）。
func lastFlagArg(argv []string) string {
	for i, a := range argv {
		if a == "--cancel-file" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
