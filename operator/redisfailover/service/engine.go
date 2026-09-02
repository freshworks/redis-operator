package service

import (
	redisfailoverv1 "github.com/freshworks/redis-operator/api/redisfailover/v1"
)

// DatabaseEngineProvider resolves Redis vs Valkey container binaries and CLI auth for shell snippets.
type DatabaseEngineProvider interface {
	ServerBinary() string
	CLIBinary() string
	// CLIAuthEnvName is exported before invoking the CLI when REDIS_PASSWORD is set (REDISCLI_AUTH).
	CLIAuthEnvName() string
}

type redisEngine struct{}

func (redisEngine) ServerBinary() string   { return "redis-server" }
func (redisEngine) CLIBinary() string      { return "redis-cli" }
func (redisEngine) CLIAuthEnvName() string { return "REDISCLI_AUTH" }

type valkeyEngine struct{}

func (valkeyEngine) ServerBinary() string { return "valkey-server" }
func (valkeyEngine) CLIBinary() string    { return "valkey-cli" }

// valkey-cli reads REDISCLI_AUTH on every release; VALKEYCLI_AUTH is an alias
// added only in valkey 9.0 (with REDISCLI_AUTH kept as a permanent fallback),
// so REDISCLI_AUTH is the one that authenticates on the 7.2 and 8.x images
// this operator can run — including its own default valkey image.
func (valkeyEngine) CLIAuthEnvName() string { return "REDISCLI_AUTH" }

// EngineFor returns the engine implementation for pod generation. Empty or Redis uses Redis binaries; Valkey uses Valkey binaries.
func EngineFor(rf *redisfailoverv1.RedisFailover) DatabaseEngineProvider {
	switch rf.Spec.Engine {
	case redisfailoverv1.ValkeyEngine:
		return valkeyEngine{}
	default:
		return redisEngine{}
	}
}
