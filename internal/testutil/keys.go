package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/platacard/asynq/internal/base"
	"github.com/redis/go-redis/v9"
)

// KeyDump is a snapshot of a single redis key.
type KeyDump struct {
	// Type is the redis type of the key (string, list, set, zset, hash).
	Type string
	// Value is a canonical JSON representation of the key's content.
	// Set members and hash fields are sorted, and task messages stored in
	// task hashes are decoded, so that two dumps can be compared as strings.
	Value string
	// HasTTL reports whether the key has an expiration set.
	HasTTL bool
}

// ScanKeys returns all keys in the database, sorted.
// With a cluster client, keys from all master nodes are returned.
func ScanKeys(tb testing.TB, r redis.UniversalClient) []string {
	tb.Helper()
	ctx := context.Background()
	var (
		mu   sync.Mutex
		keys []string
	)
	scan := func(ctx context.Context, c redis.Cmdable) error {
		iter := c.Scan(ctx, 0, "*", 1000).Iterator()
		for iter.Next(ctx) {
			mu.Lock()
			keys = append(keys, iter.Val())
			mu.Unlock()
		}
		return iter.Err()
	}
	var err error
	switch c := r.(type) {
	case *redis.ClusterClient:
		err = c.ForEachMaster(ctx, func(ctx context.Context, c *redis.Client) error {
			return scan(ctx, c)
		})
	default:
		err = scan(ctx, r)
	}
	if err != nil {
		tb.Fatalf("scan keys: %v", err)
	}
	sort.Strings(keys)
	return keys
}

// DumpKeys returns a snapshot of all keys in the database.
func DumpKeys(tb testing.TB, r redis.UniversalClient) map[string]KeyDump {
	tb.Helper()
	ctx := context.Background()
	keys := ScanKeys(tb, r)

	// Read types and TTLs, and then values, using pipelines to keep the number
	// of round trips independent of the number of keys.
	typeCmds := make([]*redis.StatusCmd, len(keys))
	ttlCmds := make([]*redis.DurationCmd, len(keys))
	if _, err := r.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, key := range keys {
			typeCmds[i] = p.Type(ctx, key)
			ttlCmds[i] = p.PTTL(ctx, key)
		}
		return nil
	}); err != nil {
		tb.Fatalf("read types of keys: %v", err)
	}
	valueCmds := make([]redis.Cmder, len(keys))
	if _, err := r.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, key := range keys {
			switch typ := typeCmds[i].Val(); typ {
			case "string":
				valueCmds[i] = p.Get(ctx, key)
			case "list":
				valueCmds[i] = p.LRange(ctx, key, 0, -1)
			case "set":
				valueCmds[i] = p.SMembers(ctx, key)
			case "zset":
				valueCmds[i] = p.ZRangeWithScores(ctx, key, 0, -1)
			case "hash":
				valueCmds[i] = p.HGetAll(ctx, key)
			default:
				tb.Fatalf("key %s has unexpected type %q", key, typ)
			}
		}
		return nil
	}); err != nil {
		tb.Fatalf("read values of keys: %v", err)
	}

	res := make(map[string]KeyDump, len(keys))
	for i, key := range keys {
		var value interface{}
		switch cmd := valueCmds[i].(type) {
		case *redis.StringCmd:
			value = cmd.Val()
		case *redis.StringSliceCmd:
			members := cmd.Val()
			if typeCmds[i].Val() == "set" {
				sort.Strings(members)
			}
			value = members
		case *redis.ZSliceCmd:
			value = cmd.Val()
		case *redis.MapStringStringCmd:
			var err error
			if value, err = decodeHash(cmd.Val()); err != nil {
				tb.Fatalf("key %s: %v", key, err)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			tb.Fatalf("encode value of key %s: %v", key, err)
		}
		res[key] = KeyDump{Type: typeCmds[i].Val(), Value: string(encoded), HasTTL: ttlCmds[i].Val() > 0}
	}
	return res
}

// decodeHash decodes the task message stored in the "msg" field of a task hash,
// so that keys referenced by the message (e.g. its unique key) are visible in the dump.
func decodeHash(fields map[string]string) (map[string]interface{}, error) {
	res := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		res[k] = v
	}
	if data, ok := fields["msg"]; ok {
		msg, err := base.DecodeMessage([]byte(data))
		if err != nil {
			return nil, fmt.Errorf("decode task message: %w", err)
		}
		res["msg"] = msg
	}
	return res, nil
}

// KeysUnderPrefix returns the dumps of the keys that belong to the given
// redis key prefix, with the prefix replaced by base.GlobalPrefix in key names
// and values. This allows comparing the data written under a custom prefix with
// the data written under the global prefix.
//
// A key belongs to a prefix if it starts with "<prefix>:" and does not belong
// to a longer prefix from others.
func KeysUnderPrefix(dump map[string]KeyDump, prefix string, others ...string) map[string]KeyDump {
	res := make(map[string]KeyDump)
	for key, d := range dump {
		if OwnerPrefix(key, append([]string{prefix}, others...)...) != prefix {
			continue
		}
		d.Value = ReplacePrefix(d.Value, prefix)
		res[ReplacePrefix(key, prefix)] = d
	}
	return res
}

// OwnerPrefix returns the longest of the given prefixes that the key belongs to,
// or an empty string if the key does not belong to any of them.
func OwnerPrefix(key string, prefixes ...string) string {
	var owner string
	for _, p := range prefixes {
		if strings.HasPrefix(key, p+":") && len(p) > len(owner) {
			owner = p
		}
	}
	return owner
}

// ReplacePrefix replaces all occurrences of "<prefix>:" in s with "<base.GlobalPrefix>:".
func ReplacePrefix(s, prefix string) string {
	if prefix == base.GlobalPrefix {
		return s
	}
	return strings.ReplaceAll(s, prefix+":", base.GlobalPrefix+":")
}
