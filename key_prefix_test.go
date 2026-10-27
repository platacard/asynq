package asynq

import (
	"context"
	"testing"
	"time"

	"github.com/platacard/asynq/internal/base"
)

func TestRedisKeyPrefixIsolatesInstances(t *testing.T) {
	r := setup(t)
	defer r.Close()

	const (
		prefixA = "svc-a:asynq"
		prefixB = "svc-b:asynq"
		qname   = "default"
	)
	ctx := context.Background()

	clientA := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	clientB := NewClientFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	inspectorA := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixA))
	inspectorB := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefixB))
	inspectorGlobal := NewInspectorFromRedisClient(r)

	infoA, err := clientA.Enqueue(NewTask("a", nil), Unique(time.Hour))
	if err != nil {
		t.Fatalf("clientA.Enqueue: %v", err)
	}
	infoB, err := clientB.Enqueue(NewTask("b", nil))
	if err != nil {
		t.Fatalf("clientB.Enqueue: %v", err)
	}

	for _, key := range []string{
		base.NewKeys(prefixA).TaskKey(qname, infoA.ID),
		base.NewKeys(prefixA).PendingKey(qname),
		base.NewKeys(prefixA).UniqueKey(qname, "a", nil),
		base.NewKeys(prefixB).TaskKey(qname, infoB.ID),
		base.NewKeys(prefixB).PendingKey(qname),
	} {
		if n := r.Exists(ctx, key).Val(); n != 1 {
			t.Errorf("key %q does not exist", key)
		}
	}
	if n := r.Exists(ctx, base.PendingKey(qname)).Val(); n != 0 {
		t.Errorf("key %q exists, want tasks to be written only under custom prefixes", base.PendingKey(qname))
	}

	if _, err := inspectorA.GetTaskInfo(qname, infoA.ID); err != nil {
		t.Errorf("inspectorA.GetTaskInfo(own task): %v", err)
	}
	if _, err := inspectorA.GetTaskInfo(qname, infoB.ID); err == nil {
		t.Errorf("inspectorA.GetTaskInfo(task of prefix B) succeeded, want error")
	}
	if _, err := inspectorB.GetTaskInfo(qname, infoB.ID); err != nil {
		t.Errorf("inspectorB.GetTaskInfo(own task): %v", err)
	}
	if queues, _ := inspectorGlobal.Queues(); len(queues) != 0 {
		t.Errorf("inspector with global prefix sees queues %v, want none", queues)
	}

	scheduledB, err := clientB.Enqueue(NewTask("b-scheduled", nil), ProcessIn(time.Hour))
	if err != nil {
		t.Fatalf("clientB.Enqueue(scheduled): %v", err)
	}
	if err := inspectorB.UpdateTaskPayload(qname, scheduledB.ID, []byte("updated")); err != nil {
		t.Errorf("inspectorB.UpdateTaskPayload(own task): %v", err)
	} else if info, err := inspectorB.GetTaskInfo(qname, scheduledB.ID); err != nil {
		t.Errorf("inspectorB.GetTaskInfo after update: %v", err)
	} else if string(info.Payload) != "updated" {
		t.Errorf("payload after UpdateTaskPayload = %q, want %q", info.Payload, "updated")
	}

	processed := make(chan string, 2)
	srv := NewServerFromRedisClient(r, Config{
		Concurrency:    1,
		LogLevel:       testLogLevel,
		RedisKeyPrefix: prefixA,
	})
	if err := srv.Start(HandlerFunc(func(ctx context.Context, task *Task) error {
		processed <- task.Type()
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	select {
	case typ := <-processed:
		if typ != "a" {
			t.Errorf("server with prefix A processed task %q, want %q", typ, "a")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server with prefix A did not process its task")
	}
	select {
	case typ := <-processed:
		t.Errorf("server with prefix A processed unexpected task %q", typ)
	case <-time.After(2 * time.Second):
	}
	if _, err := inspectorB.GetTaskInfo(qname, infoB.ID); err != nil {
		t.Errorf("task of prefix B was touched by server with prefix A: %v", err)
	}
}

func TestSchedulerRedisKeyPrefix(t *testing.T) {
	r := setup(t)
	defer r.Close()

	const prefix = "svc-sched:asynq"
	scheduler := NewSchedulerFromRedisClient(r, &SchedulerOpts{
		LogLevel:       testLogLevel,
		RedisKeyPrefix: prefix,
	})
	if _, err := scheduler.Register("@every 1s", NewTask("sched", nil)); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown()

	inspector := NewInspectorFromRedisClient(r, WithRedisKeyPrefix(prefix))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := inspector.GetQueueInfo("default"); err == nil && info.Pending > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("scheduler with custom prefix did not enqueue a task under that prefix")
}
