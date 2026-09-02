package service

import (
	"testing"

	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEngineFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		engine redisfailoverv1.DatabaseEngine
		server string
		cli    string
		auth   string
	}{
		{"omitted is redis", "", "redis-server", "redis-cli", "REDISCLI_AUTH"},
		{"redis", redisfailoverv1.RedisEngine, "redis-server", "redis-cli", "REDISCLI_AUTH"},
		{"valkey", redisfailoverv1.ValkeyEngine, "valkey-server", "valkey-cli", "REDISCLI_AUTH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rf := &redisfailoverv1.RedisFailover{
				ObjectMeta: metav1.ObjectMeta{Name: "x"},
				Spec: redisfailoverv1.RedisFailoverSpec{
					Engine: tc.engine,
				},
			}
			eng := EngineFor(rf)
			assert.Equal(t, tc.server, eng.ServerBinary())
			assert.Equal(t, tc.cli, eng.CLIBinary())
			assert.Equal(t, tc.auth, eng.CLIAuthEnvName())
		})
	}
}

func TestGetRedisCommandUsesEngine(t *testing.T) {
	t.Parallel()
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Engine: redisfailoverv1.ValkeyEngine,
		},
	}
	cmd := getRedisCommand(rf, EngineFor(rf))
	assert.Equal(t, []string{"valkey-server", "/redis/redis.conf"}, cmd)

	rf.Spec.Engine = redisfailoverv1.RedisEngine
	cmd = getRedisCommand(rf, EngineFor(rf))
	assert.Equal(t, []string{"redis-server", "/redis/redis.conf"}, cmd)
}

func TestGetSentinelCommandUsesEngine(t *testing.T) {
	t.Parallel()
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Engine: redisfailoverv1.ValkeyEngine,
		},
	}
	cmd := getSentinelCommand(rf, EngineFor(rf))
	assert.Equal(t, []string{"valkey-server", "/redis/sentinel.conf", "--sentinel"}, cmd)
}
