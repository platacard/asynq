// Copyright 2020 Kentaro Hibino. All rights reserved.
// Use of this source code is governed by a MIT license
// that can be found in the LICENSE file.

package base

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"time"
)

// GlobalPrefix is the prefix for all redis keys used by asynq.
var GlobalPrefix = "asynq"

// Keys builds redis keys used by asynq under a specific prefix.
//
// The zero value builds keys under GlobalPrefix.
type Keys struct {
	prefix string
}

// NewKeys returns Keys that build redis keys under the given prefix.
// If prefix is empty, GlobalPrefix is used.
func NewKeys(prefix string) Keys {
	return Keys{prefix: prefix}
}

// Prefix returns the prefix used for all redis keys.
func (k Keys) Prefix() string {
	if k.prefix == "" {
		return GlobalPrefix
	}
	return k.prefix
}

// AllServers returns a redis key for the set of all servers.
func (k Keys) AllServers() string {
	return fmt.Sprintf("%s:servers", k.Prefix()) // ZSET
}

// AllWorkers returns a redis key for the set of all workers.
func (k Keys) AllWorkers() string {
	return fmt.Sprintf("%s:workers", k.Prefix()) // ZSET
}

// AllSchedulers returns a redis key for the set of all schedulers.
func (k Keys) AllSchedulers() string {
	return fmt.Sprintf("%s:schedulers", k.Prefix()) // ZSET
}

// AllQueues returns a redis key for the set of all queue names.
func (k Keys) AllQueues() string {
	return fmt.Sprintf("%s:queues", k.Prefix()) // SET
}

// CancelChannel returns the name of the pub/sub channel for task cancelation.
func (k Keys) CancelChannel() string {
	return fmt.Sprintf("%s:cancel", k.Prefix()) // PubSub channel
}

// QueueKeyPrefix returns a prefix for all keys in the given queue.
func (k Keys) QueueKeyPrefix(qname string) string {
	return fmt.Sprintf("%s:{%s}:", k.Prefix(), qname)
}

// TaskKeyPrefix returns a prefix for task key.
func (k Keys) TaskKeyPrefix(qname string) string {
	return k.QueueKeyPrefix(qname) + "t:"
}

// TaskKey returns a redis key for the given task message.
func (k Keys) TaskKey(qname, id string) string {
	return k.TaskKeyPrefix(qname) + id
}

// PendingKey returns a redis key for the given queue name.
func (k Keys) PendingKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "pending"
}

// ActiveKey returns a redis key for the active tasks.
func (k Keys) ActiveKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "active"
}

// ScheduledKey returns a redis key for the scheduled tasks.
func (k Keys) ScheduledKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "scheduled"
}

// RetryKey returns a redis key for the retry tasks.
func (k Keys) RetryKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "retry"
}

// ArchivedKey returns a redis key for the archived tasks.
func (k Keys) ArchivedKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "archived"
}

// LeaseKey returns a redis key for the lease.
func (k Keys) LeaseKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "lease"
}

// CompletedKey returns a redis key for the completed tasks.
func (k Keys) CompletedKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "completed"
}

// PausedKey returns a redis key to indicate that the given queue is paused.
func (k Keys) PausedKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "paused"
}

// ProcessedTotalKey returns a redis key for total processed count for the given queue.
func (k Keys) ProcessedTotalKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "processed"
}

// FailedTotalKey returns a redis key for total failure count for the given queue.
func (k Keys) FailedTotalKey(qname string) string {
	return k.QueueKeyPrefix(qname) + "failed"
}

// ProcessedKey returns a redis key for processed count for the given day for the queue.
func (k Keys) ProcessedKey(qname string, t time.Time) string {
	return k.QueueKeyPrefix(qname) + "processed:" + t.UTC().Format("2006-01-02")
}

// FailedKey returns a redis key for failure count for the given day for the queue.
func (k Keys) FailedKey(qname string, t time.Time) string {
	return k.QueueKeyPrefix(qname) + "failed:" + t.UTC().Format("2006-01-02")
}

// ServerInfoKey returns a redis key for process info.
func (k Keys) ServerInfoKey(hostname string, pid int, serverID string) string {
	return fmt.Sprintf("%s:servers:{%s:%d:%s}", k.Prefix(), hostname, pid, serverID)
}

// WorkersKey returns a redis key for the workers given hostname, pid, and server ID.
func (k Keys) WorkersKey(hostname string, pid int, serverID string) string {
	return fmt.Sprintf("%s:workers:{%s:%d:%s}", k.Prefix(), hostname, pid, serverID)
}

// SchedulerEntriesKey returns a redis key for the scheduler entries given scheduler ID.
func (k Keys) SchedulerEntriesKey(schedulerID string) string {
	return fmt.Sprintf("%s:schedulers:{%s}", k.Prefix(), schedulerID)
}

// SchedulerHistoryKey returns a redis key for the scheduler's history for the given entry.
func (k Keys) SchedulerHistoryKey(entryID string) string {
	return fmt.Sprintf("%s:scheduler_history:%s", k.Prefix(), entryID)
}

// UniqueKey returns a redis key with the given type, payload, and queue name.
func (k Keys) UniqueKey(qname, tasktype string, payload []byte) string {
	if payload == nil {
		return k.QueueKeyPrefix(qname) + "unique:" + tasktype + ":"
	}
	checksum := md5.Sum(payload)
	return k.QueueKeyPrefix(qname) + "unique:" + tasktype + ":" + hex.EncodeToString(checksum[:])
}

// GroupKeyPrefix returns a prefix for group key.
func (k Keys) GroupKeyPrefix(qname string) string {
	return k.QueueKeyPrefix(qname) + "g:"
}

// GroupKey returns a redis key used to group tasks belong in the same group.
func (k Keys) GroupKey(qname, gkey string) string {
	return k.GroupKeyPrefix(qname) + gkey
}

// AggregationSetKey returns a redis key used for an aggregation set.
func (k Keys) AggregationSetKey(qname, gname, setID string) string {
	return k.GroupKey(qname, gname) + ":" + setID
}

// AllGroups return a redis key used to store all group keys used in a given queue.
func (k Keys) AllGroups(qname string) string {
	return k.QueueKeyPrefix(qname) + "groups"
}

// AllAggregationSets returns a redis key used to store all aggregation sets (set of tasks staged to be aggregated)
// in a given queue.
func (k Keys) AllAggregationSets(qname string) string {
	return k.QueueKeyPrefix(qname) + "aggregation_sets"
}

// Package-level key builders below build keys under GlobalPrefix.
// They are equivalent to calling the same methods on the zero value of Keys.

// AllServers is Keys.AllServers with GlobalPrefix.
func AllServers() string { return Keys{}.AllServers() }

// AllWorkers is Keys.AllWorkers with GlobalPrefix.
func AllWorkers() string { return Keys{}.AllWorkers() }

// AllSchedulers is Keys.AllSchedulers with GlobalPrefix.
func AllSchedulers() string { return Keys{}.AllSchedulers() }

// AllQueues is Keys.AllQueues with GlobalPrefix.
func AllQueues() string { return Keys{}.AllQueues() }

// CancelChannel is Keys.CancelChannel with GlobalPrefix.
func CancelChannel() string { return Keys{}.CancelChannel() }

// QueueKeyPrefix is Keys.QueueKeyPrefix with GlobalPrefix.
func QueueKeyPrefix(qname string) string { return Keys{}.QueueKeyPrefix(qname) }

// TaskKeyPrefix is Keys.TaskKeyPrefix with GlobalPrefix.
func TaskKeyPrefix(qname string) string { return Keys{}.TaskKeyPrefix(qname) }

// TaskKey is Keys.TaskKey with GlobalPrefix.
func TaskKey(qname, id string) string { return Keys{}.TaskKey(qname, id) }

// PendingKey is Keys.PendingKey with GlobalPrefix.
func PendingKey(qname string) string { return Keys{}.PendingKey(qname) }

// ActiveKey is Keys.ActiveKey with GlobalPrefix.
func ActiveKey(qname string) string { return Keys{}.ActiveKey(qname) }

// ScheduledKey is Keys.ScheduledKey with GlobalPrefix.
func ScheduledKey(qname string) string { return Keys{}.ScheduledKey(qname) }

// RetryKey is Keys.RetryKey with GlobalPrefix.
func RetryKey(qname string) string { return Keys{}.RetryKey(qname) }

// ArchivedKey is Keys.ArchivedKey with GlobalPrefix.
func ArchivedKey(qname string) string { return Keys{}.ArchivedKey(qname) }

// LeaseKey is Keys.LeaseKey with GlobalPrefix.
func LeaseKey(qname string) string { return Keys{}.LeaseKey(qname) }

// CompletedKey is Keys.CompletedKey with GlobalPrefix.
func CompletedKey(qname string) string { return Keys{}.CompletedKey(qname) }

// PausedKey is Keys.PausedKey with GlobalPrefix.
func PausedKey(qname string) string { return Keys{}.PausedKey(qname) }

// ProcessedTotalKey is Keys.ProcessedTotalKey with GlobalPrefix.
func ProcessedTotalKey(qname string) string { return Keys{}.ProcessedTotalKey(qname) }

// FailedTotalKey is Keys.FailedTotalKey with GlobalPrefix.
func FailedTotalKey(qname string) string { return Keys{}.FailedTotalKey(qname) }

// ProcessedKey is Keys.ProcessedKey with GlobalPrefix.
func ProcessedKey(qname string, t time.Time) string { return Keys{}.ProcessedKey(qname, t) }

// FailedKey is Keys.FailedKey with GlobalPrefix.
func FailedKey(qname string, t time.Time) string { return Keys{}.FailedKey(qname, t) }

// ServerInfoKey is Keys.ServerInfoKey with GlobalPrefix.
func ServerInfoKey(hostname string, pid int, serverID string) string {
	return Keys{}.ServerInfoKey(hostname, pid, serverID)
}

// WorkersKey is Keys.WorkersKey with GlobalPrefix.
func WorkersKey(hostname string, pid int, serverID string) string {
	return Keys{}.WorkersKey(hostname, pid, serverID)
}

// SchedulerEntriesKey is Keys.SchedulerEntriesKey with GlobalPrefix.
func SchedulerEntriesKey(schedulerID string) string { return Keys{}.SchedulerEntriesKey(schedulerID) }

// SchedulerHistoryKey is Keys.SchedulerHistoryKey with GlobalPrefix.
func SchedulerHistoryKey(entryID string) string { return Keys{}.SchedulerHistoryKey(entryID) }

// UniqueKey is Keys.UniqueKey with GlobalPrefix.
func UniqueKey(qname, tasktype string, payload []byte) string {
	return Keys{}.UniqueKey(qname, tasktype, payload)
}

// GroupKeyPrefix is Keys.GroupKeyPrefix with GlobalPrefix.
func GroupKeyPrefix(qname string) string { return Keys{}.GroupKeyPrefix(qname) }

// GroupKey is Keys.GroupKey with GlobalPrefix.
func GroupKey(qname, gkey string) string { return Keys{}.GroupKey(qname, gkey) }

// AggregationSetKey is Keys.AggregationSetKey with GlobalPrefix.
func AggregationSetKey(qname, gname, setID string) string {
	return Keys{}.AggregationSetKey(qname, gname, setID)
}

// AllGroups is Keys.AllGroups with GlobalPrefix.
func AllGroups(qname string) string { return Keys{}.AllGroups(qname) }

// AllAggregationSets is Keys.AllAggregationSets with GlobalPrefix.
func AllAggregationSets(qname string) string { return Keys{}.AllAggregationSets(qname) }
