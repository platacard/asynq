package asynq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/platacard/asynq/internal/base"
	"github.com/platacard/asynq/internal/rdb"
	h "github.com/platacard/asynq/internal/testutil"
	"github.com/platacard/asynq/internal/timeutil"
	"github.com/redis/go-redis/v9"
)

// End-to-end tests of per-instance redis key prefixes. Each test runs several
// instances with different prefixes against one redis database and checks
// that they don't see or touch each other's data, and that every key written
// to redis is under the prefix of the instance that wrote it.

const (
	prefixA = "svc-a:asynq"
	prefixB = "svc-b:asynq"
)

// setupPrefixTest returns a redis client for the test. The client is closed
// after all other cleanups (e.g. server shutdowns) have run.
func setupPrefixTest(t *testing.T) redis.UniversalClient {
	t.Helper()
	r := setup(t)
	t.Cleanup(func() { r.Close() })
	return r
}

// assertKeysUnderPrefixes reports every key in redis that is not under one of the given prefixes.
func assertKeysUnderPrefixes(t *testing.T, r redis.UniversalClient, prefixes ...string) {
	t.Helper()
	for _, key := range h.ScanKeys(t, r) {
		if h.OwnerPrefix(key, prefixes...) == "" {
			t.Errorf("key %q is not under any of the prefixes %q", key, prefixes)
		}
	}
}

// keysUnderPrefix returns a snapshot of the keys under the given prefix.
func keysUnderPrefix(t *testing.T, r redis.UniversalClient, prefix string) map[string]h.KeyDump {
	t.Helper()
	return h.KeysUnderPrefix(h.DumpKeys(t, r), prefix)
}

// waitUntil polls cond until it returns true, and fails the test if it does not within timeout.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, format string, args ...interface{}) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until "+format, args...)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startPrefixedServer starts a server that uses the given prefix and shuts it down on cleanup.
func startPrefixedServer(t *testing.T, r redis.UniversalClient, prefix string, cfg Config, handler Handler) {
	t.Helper()
	cfg.RedisKeyPrefix = prefix
	cfg.LogLevel = testLogLevel
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = time.Second
	}
	srv := NewServerFromRedisClient(r, cfg)
	if err := srv.Start(handler); err != nil {
		t.Fatalf("start server with prefix %q: %v", prefix, err)
	}
	t.Cleanup(srv.Shutdown)
}

// processedTasks records the tasks processed by a handler.
type processedTasks struct {
	mu    sync.Mutex
	tasks []string // "<type>:<payload>"
}

func (p *processedTasks) record(task *Task) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks = append(p.tasks, task.Type()+":"+string(task.Payload()))
}

func (p *processedTasks) get() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := append([]string(nil), p.tasks...)
	sort.Strings(res)
	return res
}

func (p *processedTasks) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tasks)
}

func (p *processedTasks) handler() Handler {
	return HandlerFunc(func(ctx context.Context, task *Task) error {
		p.record(task)
		return nil
	})
}

func TestKeyPrefixServersProcessOnlyOwnTasks(t *testing.T) {
	r := setupPrefixTest(t)

	instances := []struct {
		label  string
		prefix string // empty means the global prefix
	}{
		{"A", prefixA},
		{"B", prefixB},
		{"G", ""},
	}
	queues := map[string]int{"default": 1, "critical": 2}
	processed := make(map[string]*processedTasks)
	for _, inst := range instances {
		client := NewClientFromRedisClient(r, WithRedisKeyPrefix(inst.prefix))
		// The same task IDs are used under every prefix.
		if _, err := client.Enqueue(NewTask("task", []byte(inst.label)), TaskID("same-id")); err != nil {
			t.Fatalf("client %s: Enqueue: %v", inst.label, err)
		}
		if _, err := client.Enqueue(NewTask("critical_task", []byte(inst.label)), TaskID("same-id"), Queue("critical")); err != nil {
			t.Fatalf("client %s: Enqueue to critical queue: %v", inst.label, err)
		}
		if _, err := client.Enqueue(NewTask("task", []byte(inst.label)), TaskID("same-id")); !errors.Is(err, ErrTaskIDConflict) {
			t.Errorf("client %s: Enqueue with a duplicate task ID returned %v, want %v", inst.label, err, ErrTaskIDConflict)
		}
	}
	for _, inst := range instances {
		processed[inst.label] = &processedTasks{}
		startPrefixedServer(t, r, inst.prefix, Config{Queues: queues}, processed[inst.label].handler())
	}

	for _, inst := range instances {
		p := processed[inst.label]
		waitUntil(t, 10*time.Second, func() bool { return p.count() >= 2 }, "server %s processes its tasks", inst.label)
	}
	// Give the servers a chance to process tasks that are not theirs.
	time.Sleep(time.Second)
	for _, inst := range instances {
		want := []string{"critical_task:" + inst.label, "task:" + inst.label}
		if diff := cmp.Diff(want, processed[inst.label].get()); diff != "" {
			t.Errorf("server %s processed unexpected tasks (-want +got):\n%s", inst.label, diff)
		}

		inspector := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(inst.prefix))
		for _, qname := range []string{"default", "critical"} {
			waitUntil(t, 5*time.Second, func() bool {
				info, err := inspector.GetQueueInfo(qname)
				return err == nil && info.Processed == 1 && info.Size == 0
			}, "inspector %s reports queue %q as processed", inst.label, qname)
		}
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB, base.GlobalPrefix)
}

func TestKeyPrefixUniqueTasksAreIsolated(t *testing.T) {
	r := setupPrefixTest(t)
	clientA := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	clientB := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixB))

	tests := []struct {
		desc string
		task *Task
		opts []Option
	}{
		{"pending", NewTask("unique", []byte("pending")), []Option{Unique(time.Hour)}},
		{"scheduled", NewTask("unique", []byte("scheduled")), []Option{Unique(time.Hour), ProcessIn(time.Hour)}},
		{"grouped", NewTask("unique", []byte("grouped")), []Option{Unique(time.Hour), Group("g")}},
	}
	for _, tc := range tests {
		for label, client := range map[string]*Client{"A": clientA, "B": clientB} {
			if _, err := client.Enqueue(tc.task, tc.opts...); err != nil {
				t.Errorf("%s: client %s: first Enqueue: %v", tc.desc, label, err)
			}
			if _, err := client.Enqueue(tc.task, tc.opts...); !errors.Is(err, ErrDuplicateTask) {
				t.Errorf("%s: client %s: second Enqueue returned %v, want %v", tc.desc, label, err, ErrDuplicateTask)
			}
		}
		for _, prefix := range []string{prefixA, prefixB} {
			key := base.NewKeys(prefix).UniqueKey("default", tc.task.Type(), tc.task.Payload())
			if n := r.Exists(context.Background(), key).Val(); n != 1 {
				t.Errorf("%s: unique key %q does not exist", tc.desc, key)
			}
		}
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixRetryAndArchive(t *testing.T) {
	r := setupPrefixTest(t)
	clientA := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	clientB := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	for label, client := range map[string]*Client{"A": clientA, "B": clientB} {
		if _, err := client.Enqueue(NewTask("fail", nil), MaxRetry(1), TaskID("fail")); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
		if _, err := client.Enqueue(NewTask("skip_retry", nil), MaxRetry(5), TaskID("skip_retry")); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
	}

	startPrefixedServer(t, r, prefixA, Config{
		DelayedTaskCheckInterval: 100 * time.Millisecond,
		RetryDelayFunc:           func(int, error, *Task) time.Duration { return 0 },
	}, HandlerFunc(func(ctx context.Context, task *Task) error {
		if task.Type() == "skip_retry" {
			return fmt.Errorf("skip: %w", SkipRetry)
		}
		return errors.New("failed")
	}))

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	inspectorB := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	waitUntil(t, 10*time.Second, func() bool {
		tasks, err := inspectorA.ListArchivedTasks("default")
		return err == nil && len(tasks) == 2
	}, "both tasks with prefix A are archived")

	for _, id := range []string{"fail", "skip_retry"} {
		info, err := inspectorA.GetTaskInfo("default", id)
		if err != nil {
			t.Fatalf("inspectorA.GetTaskInfo(%q): %v", id, err)
		}
		if info.State != TaskStateArchived || info.LastErr == "" {
			t.Errorf("task %q with prefix A: state = %v, last error = %q, want archived with an error", id, info.State, info.LastErr)
		}
	}
	if info, err := inspectorA.GetTaskInfo("default", "fail"); err == nil && info.Retried != 1 {
		t.Errorf("task %q with prefix A was retried %d times, want 1", "fail", info.Retried)
	}
	waitUntil(t, 5*time.Second, func() bool {
		info, err := inspectorA.GetQueueInfo("default")
		return err == nil && info.Processed == 3 && info.Failed == 3
	}, "queue stats with prefix A count 3 processed and 3 failed attempts")

	infoB, err := inspectorB.GetQueueInfo("default")
	if err != nil {
		t.Fatalf("inspectorB.GetQueueInfo: %v", err)
	}
	if infoB.Pending != 2 || infoB.Archived != 0 || infoB.Retry != 0 || infoB.Processed != 0 || infoB.Failed != 0 {
		t.Errorf("queue with prefix B = %+v, want 2 untouched pending tasks", infoB)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixScheduledTasksAreForwarded(t *testing.T) {
	r := setupPrefixTest(t)
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix))
		if _, err := client.Enqueue(NewTask("scheduled", []byte(label)), ProcessIn(time.Second)); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
	}
	processed := &processedTasks{}
	startPrefixedServer(t, r, prefixA, Config{DelayedTaskCheckInterval: 100 * time.Millisecond}, processed.handler())

	waitUntil(t, 10*time.Second, func() bool { return processed.count() > 0 }, "server A processes the scheduled task")
	time.Sleep(time.Second)
	if diff := cmp.Diff([]string{"scheduled:A"}, processed.get()); diff != "" {
		t.Errorf("server A processed unexpected tasks (-want +got):\n%s", diff)
	}
	infoB, err := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)).GetQueueInfo("default")
	if err != nil {
		t.Fatalf("GetQueueInfo with prefix B: %v", err)
	}
	if infoB.Scheduled != 1 || infoB.Pending != 0 {
		t.Errorf("queue with prefix B = %+v, want one scheduled task", infoB)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

// joinPayloads is a group aggregator that joins the payloads of the grouped tasks.
func joinPayloads(opts ...Option) GroupAggregator {
	return GroupAggregatorFunc(func(group string, tasks []*Task) *Task {
		var payloads []string
		for _, task := range tasks {
			payloads = append(payloads, string(task.Payload()))
		}
		sort.Strings(payloads)
		return NewTask("aggregated:"+group, []byte(strings.Join(payloads, ",")), opts...)
	})
}

func TestKeyPrefixGroupAggregation(t *testing.T) {
	r := setupPrefixTest(t)
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix))
		for i := 1; i <= 3; i++ {
			if _, err := client.Enqueue(NewTask("grouped", []byte(fmt.Sprintf("%s%d", label, i))), Group("g")); err != nil {
				t.Fatalf("client %s: %v", label, err)
			}
		}
	}
	processed := &processedTasks{}
	startPrefixedServer(t, r, prefixA, Config{
		GroupGracePeriod: time.Second,
		GroupMaxSize:     3,
		GroupAggregator:  joinPayloads(),
	}, processed.handler())

	waitUntil(t, 20*time.Second, func() bool { return processed.count() > 0 }, "server A processes the aggregated task")
	if diff := cmp.Diff([]string{"aggregated:g:A1,A2,A3"}, processed.get()); diff != "" {
		t.Errorf("server A processed unexpected tasks (-want +got):\n%s", diff)
	}

	inspectorB := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	groups, err := inspectorB.Groups("default")
	if err != nil {
		t.Fatalf("inspectorB.Groups: %v", err)
	}
	if diff := cmp.Diff([]*GroupInfo{{Group: "g", Size: 3}}, groups); diff != "" {
		t.Errorf("groups with prefix B (-want +got):\n%s", diff)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

// The aggregator enqueues the aggregated task with its own client, so options
// of the aggregated task (e.g. Unique) must use the server's prefix too.
func TestKeyPrefixAggregatedUniqueTask(t *testing.T) {
	r := setupPrefixTest(t)
	ctx := context.Background()
	keysA := base.NewKeys(prefixA)
	client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	for i := 1; i <= 2; i++ {
		if _, err := client.Enqueue(NewTask("grouped", []byte(fmt.Sprint(i))), Group("g")); err != nil {
			t.Fatal(err)
		}
	}
	processed := &processedTasks{}
	// leakedKeys records the keys outside of prefix A that exist while the aggregated task is processed.
	var leakedKeys []string
	startPrefixedServer(t, r, prefixA, Config{
		GroupGracePeriod: time.Second,
		GroupMaxSize:     2,
		GroupAggregator:  joinPayloads(Unique(time.Hour), Retention(time.Hour)),
	}, HandlerFunc(func(ctx context.Context, task *Task) error {
		for _, key := range h.ScanKeys(t, r) {
			if h.OwnerPrefix(key, prefixA) == "" {
				leakedKeys = append(leakedKeys, key)
			}
		}
		processed.record(task)
		return nil
	}))

	waitUntil(t, 20*time.Second, func() bool { return processed.count() > 0 }, "server A processes the aggregated task")
	if diff := cmp.Diff([]string{"aggregated:g:1,2"}, processed.get()); diff != "" {
		t.Errorf("server A processed unexpected tasks (-want +got):\n%s", diff)
	}
	if len(leakedKeys) > 0 {
		t.Errorf("keys outside of prefix %q while the aggregated task was processed: %q", prefixA, leakedKeys)
	}

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	var completed []*TaskInfo
	waitUntil(t, 5*time.Second, func() bool {
		var err error
		completed, err = inspectorA.ListCompletedTasks("default")
		return err == nil && len(completed) == 1
	}, "the aggregated task is completed")
	data, err := r.HGet(ctx, keysA.TaskKey("default", completed[0].ID), "msg").Result()
	if err != nil {
		t.Fatalf("read the aggregated task: %v", err)
	}
	msg, err := base.DecodeMessage([]byte(data))
	if err != nil {
		t.Fatalf("decode the aggregated task: %v", err)
	}
	if want := keysA.UniqueKey("default", msg.Type, msg.Payload); msg.UniqueKey != want {
		t.Errorf("unique key of the aggregated task = %q, want %q", msg.UniqueKey, want)
	}
	assertKeysUnderPrefixes(t, r, prefixA)
}

func TestKeyPrefixCancelProcessing(t *testing.T) {
	r := setupPrefixTest(t)
	canceled := map[string]chan struct{}{"A": make(chan struct{}, 1), "B": make(chan struct{}, 1)}
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix))
		if _, err := client.Enqueue(NewTask("long", nil), TaskID("long"), MaxRetry(0)); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
		startPrefixedServer(t, r, prefix, Config{}, HandlerFunc(func(ctx context.Context, task *Task) error {
			select {
			case <-ctx.Done():
				canceled[label] <- struct{}{}
				return ctx.Err()
			case <-time.After(30 * time.Second):
				return nil
			}
		}))
	}
	inspectors := map[string]*Inspector{
		"A": NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA)),
		"B": NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)),
		"G": NewInspectorFromRedisClient(r),
	}
	for _, label := range []string{"A", "B"} {
		waitUntil(t, 10*time.Second, func() bool {
			tasks, err := inspectors[label].ListActiveTasks("default")
			return err == nil && len(tasks) == 1
		}, "server %s starts processing its task", label)
	}
	// The subscriber may not be subscribed yet right after the server starts.
	time.Sleep(500 * time.Millisecond)

	expectCanceled := func(label string) {
		t.Helper()
		select {
		case <-canceled[label]:
		case <-time.After(5 * time.Second):
			t.Fatalf("task with prefix %s was not canceled", label)
		}
	}
	expectNotCanceled := func(label string) {
		t.Helper()
		select {
		case <-canceled[label]:
			t.Fatalf("task with prefix %s was canceled", label)
		case <-time.After(time.Second):
		}
	}

	if err := inspectors["G"].CancelProcessing("long"); err != nil {
		t.Fatalf("CancelProcessing with the global prefix: %v", err)
	}
	expectNotCanceled("A")
	expectNotCanceled("B")

	if err := inspectors["A"].CancelProcessing("long"); err != nil {
		t.Fatalf("CancelProcessing with prefix A: %v", err)
	}
	expectCanceled("A")
	expectNotCanceled("B")

	if err := inspectors["B"].CancelProcessing("long"); err != nil {
		t.Fatalf("CancelProcessing with prefix B: %v", err)
	}
	expectCanceled("B")
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixServersAndWorkersInfo(t *testing.T) {
	r := setupPrefixTest(t)
	release := make(chan struct{})
	defer close(release)
	client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	if _, err := client.Enqueue(NewTask("blocking", nil), TaskID("blocking")); err != nil {
		t.Fatal(err)
	}
	srv := NewServerFromRedisClient(r, Config{
		Concurrency:     3,
		Queues:          map[string]int{"default": 1, "other": 1},
		LogLevel:        testLogLevel,
		ShutdownTimeout: time.Second,
		RedisKeyPrefix:  prefixA,
	})
	if err := srv.Start(HandlerFunc(func(ctx context.Context, task *Task) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	shutdown := sync.OnceFunc(srv.Shutdown)
	t.Cleanup(shutdown)

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	// Workers are reported by the heartbeater every 5 seconds.
	waitUntil(t, 15*time.Second, func() bool {
		servers, err := inspectorA.Servers()
		return err == nil && len(servers) == 1 && len(servers[0].ActiveWorkers) == 1
	}, "inspector A reports the server and its active worker")
	servers, _ := inspectorA.Servers()
	if got := servers[0]; got.Concurrency != 3 || got.ActiveWorkers[0].TaskID != "blocking" {
		t.Errorf("server info with prefix A = %+v, worker = %+v", got, got.ActiveWorkers[0])
	}

	for label, inspector := range map[string]*Inspector{
		"B": NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)),
		"G": NewInspectorFromRedisClient(r),
	} {
		if servers, err := inspector.Servers(); err != nil || len(servers) != 0 {
			t.Errorf("inspector %s: Servers() = %v, %v, want no servers", label, servers, err)
		}
	}
	assertKeysUnderPrefixes(t, r, prefixA)

	shutdown()
	if servers, err := inspectorA.Servers(); err != nil || len(servers) != 0 {
		t.Errorf("inspector A after shutdown: Servers() = %v, %v, want no servers", servers, err)
	}
}

func TestKeyPrefixScheduler(t *testing.T) {
	r := setupPrefixTest(t)
	// NewScheduler creates its own connection and client, unlike NewSchedulerFromRedisClient.
	scheduler := NewScheduler(getRedisConnOpt(t), &SchedulerOpts{
		LogLevel:          testLogLevel,
		HeartbeatInterval: 200 * time.Millisecond,
		RedisKeyPrefix:    prefixA,
	})
	entryID, err := scheduler.Register("@every 1s", NewTask("periodic", nil), Unique(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	shutdown := sync.OnceFunc(scheduler.Shutdown)
	t.Cleanup(shutdown)

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	waitUntil(t, 10*time.Second, func() bool {
		events, err := inspectorA.ListSchedulerEnqueueEvents(entryID)
		return err == nil && len(events) > 0
	}, "inspector A reports scheduler enqueue events")
	waitUntil(t, 5*time.Second, func() bool {
		entries, err := inspectorA.SchedulerEntries()
		return err == nil && len(entries) == 1 && entries[0].ID == entryID
	}, "inspector A reports the scheduler entry %q", entryID)
	info, err := inspectorA.GetQueueInfo("default")
	if err != nil || info.Pending == 0 {
		t.Errorf("inspectorA.GetQueueInfo() = %+v, %v, want pending periodic tasks", info, err)
	}

	for label, inspector := range map[string]*Inspector{
		"B": NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)),
		"G": NewInspectorFromRedisClient(r),
	} {
		if entries, err := inspector.SchedulerEntries(); err != nil || len(entries) != 0 {
			t.Errorf("inspector %s: SchedulerEntries() = %v, %v, want none", label, entries, err)
		}
		if events, err := inspector.ListSchedulerEnqueueEvents(entryID); err != nil || len(events) != 0 {
			t.Errorf("inspector %s: ListSchedulerEnqueueEvents() = %v, %v, want none", label, events, err)
		}
		if queues, err := inspector.Queues(); err != nil || len(queues) != 0 {
			t.Errorf("inspector %s: Queues() = %v, %v, want none", label, queues, err)
		}
	}
	assertKeysUnderPrefixes(t, r, prefixA)

	shutdown()
	if entries, err := inspectorA.SchedulerEntries(); err != nil || len(entries) != 0 {
		t.Errorf("inspector A after shutdown: SchedulerEntries() = %v, %v, want none", entries, err)
	}
}

type staticPeriodicTaskConfigs []*PeriodicTaskConfig

func (c staticPeriodicTaskConfigs) GetConfigs() ([]*PeriodicTaskConfig, error) { return c, nil }

func TestKeyPrefixPeriodicTaskManager(t *testing.T) {
	r := setupPrefixTest(t)
	mgr, err := NewPeriodicTaskManager(PeriodicTaskManagerOpts{
		PeriodicTaskConfigProvider: staticPeriodicTaskConfigs{
			{Cronspec: "@every 1s", Task: NewTask("periodic", nil)},
		},
		RedisUniversalClient: r,
		SchedulerOpts: &SchedulerOpts{
			LogLevel:          testLogLevel,
			HeartbeatInterval: 200 * time.Millisecond,
			RedisKeyPrefix:    prefixA,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	waitUntil(t, 10*time.Second, func() bool {
		info, err := inspectorA.GetQueueInfo("default")
		return err == nil && info.Pending > 0
	}, "periodic task manager enqueues tasks with prefix A")
	waitUntil(t, 5*time.Second, func() bool {
		entries, err := inspectorA.SchedulerEntries()
		return err == nil && len(entries) == 1
	}, "inspector A reports the scheduler entry")
	assertKeysUnderPrefixes(t, r, prefixA)
}

func TestKeyPrefixRecoversExpiredLeases(t *testing.T) {
	r := setupPrefixTest(t)
	// Put a task with an expired lease under each prefix: dequeue it with a
	// clock in the past, so that the lease has already expired.
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		if _, err := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix)).Enqueue(
			NewTask("orphaned", []byte(label)), TaskID("orphaned")); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
		broker := rdb.NewRDBWithPrefix(r, prefix)
		broker.SetClock(timeutil.NewSimulatedClock(time.Now().Add(-time.Hour)))
		if _, _, err := broker.Dequeue("default"); err != nil {
			t.Fatalf("dequeue task with prefix %s: %v", label, err)
		}
	}

	// The recoverer of a server runs right after the server starts.
	processed := &processedTasks{}
	startPrefixedServer(t, r, prefixA, Config{
		DelayedTaskCheckInterval: 100 * time.Millisecond,
		RetryDelayFunc:           func(int, error, *Task) time.Duration { return 0 },
	}, processed.handler())

	waitUntil(t, 10*time.Second, func() bool { return processed.count() > 0 }, "server A recovers and processes its orphaned task")
	if diff := cmp.Diff([]string{"orphaned:A"}, processed.get()); diff != "" {
		t.Errorf("server A processed unexpected tasks (-want +got):\n%s", diff)
	}
	info, err := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)).GetTaskInfo("default", "orphaned")
	if err != nil {
		t.Fatalf("GetTaskInfo with prefix B: %v", err)
	}
	if info.State != TaskStateActive {
		t.Errorf("orphaned task with prefix B is in state %v, want it to stay active", info.State)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixJanitorDeletesExpiredCompletedTasks(t *testing.T) {
	r := setupPrefixTest(t)
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		if _, err := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix)).Enqueue(
			NewTask("retained", []byte(label)), Retention(time.Second)); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
	}
	startPrefixedServer(t, r, prefixA, Config{JanitorInterval: 200 * time.Millisecond}, (&processedTasks{}).handler())
	startPrefixedServer(t, r, prefixB, Config{JanitorInterval: time.Hour}, (&processedTasks{}).handler())

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	inspectorB := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	waitUntil(t, 10*time.Second, func() bool {
		info, err := inspectorB.GetQueueInfo("default")
		return err == nil && info.Completed == 1
	}, "server B completes its task")
	waitUntil(t, 10*time.Second, func() bool {
		info, err := inspectorA.GetQueueInfo("default")
		return err == nil && info.Processed == 1 && info.Completed == 0
	}, "the janitor of server A deletes the expired completed task")
	if info, err := inspectorB.GetQueueInfo("default"); err != nil || info.Completed != 1 {
		t.Errorf("inspectorB.GetQueueInfo() = %+v, %v, want the completed task to be kept", info, err)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixPausedQueue(t *testing.T) {
	r := setupPrefixTest(t)
	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	processed := map[string]*processedTasks{"A": {}, "B": {}}
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		if _, err := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix)).Enqueue(NewTask("task", []byte(label))); err != nil {
			t.Fatalf("client %s: %v", label, err)
		}
	}
	if err := inspectorA.PauseQueue("default"); err != nil {
		t.Fatalf("PauseQueue with prefix A: %v", err)
	}
	if info, err := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB)).GetQueueInfo("default"); err != nil || info.Paused {
		t.Fatalf("queue with prefix B: %+v, %v, want it not paused", info, err)
	}
	startPrefixedServer(t, r, prefixA, Config{}, processed["A"].handler())
	startPrefixedServer(t, r, prefixB, Config{}, processed["B"].handler())

	waitUntil(t, 10*time.Second, func() bool { return processed["B"].count() > 0 }, "server B processes its task")
	time.Sleep(2 * time.Second)
	if n := processed["A"].count(); n != 0 {
		t.Fatalf("server A processed %d tasks from a paused queue", n)
	}
	if err := inspectorA.UnpauseQueue("default"); err != nil {
		t.Fatalf("UnpauseQueue with prefix A: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return processed["A"].count() > 0 }, "server A processes its task after unpause")
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixResultWriter(t *testing.T) {
	r := setupPrefixTest(t)
	client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	if _, err := client.Enqueue(NewTask("with_result", nil), TaskID("with_result"), Retention(time.Hour)); err != nil {
		t.Fatal(err)
	}
	startPrefixedServer(t, r, prefixA, Config{}, HandlerFunc(func(ctx context.Context, task *Task) error {
		_, err := task.ResultWriter().Write([]byte("result"))
		return err
	}))
	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	waitUntil(t, 10*time.Second, func() bool {
		info, err := inspectorA.GetTaskInfo("default", "with_result")
		return err == nil && info.State == TaskStateCompleted
	}, "the task is completed")
	info, err := inspectorA.GetTaskInfo("default", "with_result")
	if err != nil {
		t.Fatal(err)
	}
	if string(info.Result) != "result" {
		t.Errorf("result = %q, want %q", info.Result, "result")
	}
	assertKeysUnderPrefixes(t, r, prefixA)
}

func TestKeyPrefixInspectorOperationsAreIsolated(t *testing.T) {
	r := setupPrefixTest(t)
	// Create the same tasks under both prefixes.
	for label, prefix := range map[string]string{"A": prefixA, "B": prefixB} {
		client := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefix))
		enqueue := func(id string, opts ...Option) {
			t.Helper()
			if _, err := client.Enqueue(NewTask("task", []byte(id)), append(opts, TaskID(id))...); err != nil {
				t.Fatalf("client %s: enqueue %s: %v", label, id, err)
			}
		}
		enqueue("p1")
		enqueue("p2")
		enqueue("s1", ProcessIn(time.Hour))
		enqueue("s2", ProcessIn(time.Hour))
		enqueue("g1", Group("g"))
		enqueue("g2", Group("g"))
	}
	snapshotB := keysUnderPrefix(t, r, prefixB)
	if len(snapshotB) == 0 {
		t.Fatal("no keys under prefix B")
	}

	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	inspectorG := NewInspectorFromRedisClient(r)
	mustSucceed := func(desc string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s with prefix A: %v", desc, err)
		}
	}
	mustAffect := func(desc string, want int) func(int, error) {
		return func(n int, err error) {
			t.Helper()
			if err != nil || n != want {
				t.Errorf("%s with prefix A = %d, %v, want %d", desc, n, err, want)
			}
		}
	}

	if _, err := inspectorG.GetTaskInfo("default", "p1"); !errors.Is(err, ErrQueueNotFound) {
		t.Errorf("GetTaskInfo with the global prefix returned %v, want %v", err, ErrQueueNotFound)
	}
	mustSucceed("PauseQueue", inspectorA.PauseQueue("default"))
	mustSucceed("UpdateTaskPayload", inspectorA.UpdateTaskPayload("default", "s1", []byte("updated")))
	mustSucceed("RunTask", inspectorA.RunTask("default", "s1"))
	mustSucceed("ArchiveTask", inspectorA.ArchiveTask("default", "p1"))
	mustSucceed("DeleteTask", inspectorA.DeleteTask("default", "p1"))
	mustAffect("RunAllScheduledTasks", 1)(inspectorA.RunAllScheduledTasks("default"))
	mustAffect("ArchiveAllPendingTasks", 3)(inspectorA.ArchiveAllPendingTasks("default"))
	mustAffect("RunAllArchivedTasks", 3)(inspectorA.RunAllArchivedTasks("default"))
	mustAffect("ArchiveAllAggregatingTasks", 2)(inspectorA.ArchiveAllAggregatingTasks("default", "g"))
	mustAffect("DeleteAllArchivedTasks", 2)(inspectorA.DeleteAllArchivedTasks("default"))
	mustAffect("DeleteAllPendingTasks", 3)(inspectorA.DeleteAllPendingTasks("default"))
	mustSucceed("UnpauseQueue", inspectorA.UnpauseQueue("default"))
	mustSucceed("DeleteQueue", inspectorA.DeleteQueue("default", false))

	if queues, err := inspectorA.Queues(); err != nil || len(queues) != 0 {
		t.Errorf("inspectorA.Queues() = %v, %v, want none", queues, err)
	}
	if diff := cmp.Diff(snapshotB, keysUnderPrefix(t, r, prefixB)); diff != "" {
		t.Errorf("operations with prefix A changed the data under prefix B (-before +after):\n%s", diff)
	}
	assertKeysUnderPrefixes(t, r, prefixA, prefixB)
}

func TestKeyPrefixConstructorsWithRedisConnOpt(t *testing.T) {
	r := setupPrefixTest(t)
	client := NewClient(getRedisConnOpt(t), WithRedisKeyPrefix(prefixA))
	defer client.Close()
	inspector := NewInspector(getRedisConnOpt(t), WithRedisKeyPrefix(prefixA))
	defer inspector.Close()

	if _, err := client.Enqueue(NewTask("task", nil), TaskID("id"), Retention(time.Hour)); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(getRedisConnOpt(t), Config{LogLevel: testLogLevel, RedisKeyPrefix: prefixA})
	if err := srv.Start((&processedTasks{}).handler()); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	waitUntil(t, 10*time.Second, func() bool {
		info, err := inspector.GetTaskInfo("default", "id")
		return err == nil && info.State == TaskStateCompleted
	}, "the task is completed")
	assertKeysUnderPrefixes(t, r, prefixA)
}

// An empty prefix means the global prefix, and the global prefix can also be set explicitly.
func TestKeyPrefixGlobalPrefixInteroperability(t *testing.T) {
	r := setupPrefixTest(t)
	explicit := NewClientFromRedisClient(r, WithRedisKeyPrefix(base.GlobalPrefix))
	empty := NewClientFromRedisClient(r, WithRedisKeyPrefix(""))
	if _, err := explicit.Enqueue(NewTask("task", []byte("explicit"))); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Enqueue(NewTask("task", []byte("empty"))); err != nil {
		t.Fatal(err)
	}
	processed := &processedTasks{}
	startPrefixedServer(t, r, "", Config{}, processed.handler())
	waitUntil(t, 10*time.Second, func() bool { return processed.count() == 2 }, "server with the global prefix processes both tasks")
	if diff := cmp.Diff([]string{"task:empty", "task:explicit"}, processed.get()); diff != "" {
		t.Errorf("processed tasks (-want +got):\n%s", diff)
	}
	for _, inspector := range []*Inspector{
		NewInspectorFromRedisClient(r, WithRedisKeyPrefix(base.GlobalPrefix)),
		NewInspectorFromRedisClient(r, WithRedisKeyPrefix("")),
		NewInspectorFromRedisClient(r),
	} {
		waitUntil(t, 5*time.Second, func() bool {
			info, err := inspector.GetQueueInfo("default")
			return err == nil && info.Processed == 2
		}, "inspector reports both tasks as processed")
	}
	assertKeysUnderPrefixes(t, r, base.GlobalPrefix)
}
