//go:build integration

package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestRedisACLUsernameEndToEnd(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	const defaultPassword = "default-password/@%"
	const namedUser = "named:user/@%"
	const namedPassword = "named-password/@%"
	redisConfig := fmt.Sprintf(
		"user default on +@all ~* >%s\nuser %s on +@all ~* >%s\n",
		defaultPassword,
		namedUser,
		namedPassword,
	)
	configPath := filepath.Join(t.TempDir(), "redis.conf")
	if err := os.WriteFile(configPath, []byte(redisConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:8.4-alpine", tcredis.WithConfigFile(configPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mappedPort, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	port := mappedPort.Int()

	if err := TestRedisConnection(&RedisConfig{Host: host, Port: port, Password: defaultPassword}); err != nil {
		t.Fatal(err)
	}
	if err := TestRedisConnection(&RedisConfig{Host: host, Port: port, Username: namedUser, Password: namedPassword}); err != nil {
		t.Fatal(err)
	}
	if err := TestRedisConnection(&RedisConfig{Host: host, Port: port, Username: "wrong-user", Password: namedPassword}); err == nil {
		t.Fatal("wrong Redis ACL username unexpectedly authenticated")
	}

	runtimeCfg := &config.Config{Redis: config.RedisConfig{
		Host: host, Port: port, Username: namedUser, Password: namedPassword,
	}}
	runtimeClient := repository.InitRedis(runtimeCfg)
	t.Cleanup(func() { _ = runtimeClient.Close() })
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := runtimeClient.Ping(pingCtx).Err(); err != nil {
		t.Fatal(err)
	}
}
