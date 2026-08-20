package cluster

import (
	"strings"
	"testing"
)

func TestRedisSessionKeysShareReadableClusterHashTag(t *testing.T) {
	runtime := &RedisRuntime{clusterID: "cluster-a"}
	owner := runtime.ownerKey("tenant:a", "session|1")
	fence := runtime.fenceKey("tenant:a", "session|1")
	if !strings.Contains(owner, "{tenant%3Aa|session%7C1}") ||
		strings.TrimSuffix(owner, ":owner") != strings.TrimSuffix(fence, ":fence") {
		t.Fatalf("owner and fence keys do not share the expected hash tag: %s %s", owner, fence)
	}
}
