package setup

import (
	"bufio"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

func TestPromptOptionalCredentialPreservesSpecialCharactersAndSpaces(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(" acl:user/@% \r\n"))
	if got := promptOptionalCredential(reader, "Redis Username"); got != " acl:user/@% " {
		t.Fatalf("promptOptionalCredential() = %q", got)
	}
}

func TestBuildSetupRedisOptionsPreservesCredentialsAndTransport(t *testing.T) {
	cfg := &RedisConfig{
		Host:      "redis.example.com",
		Port:      6380,
		Username:  " acl:user/@% ",
		Password:  " password/@% ",
		DB:        3,
		EnableTLS: true,
	}

	opts := buildSetupRedisOptions(cfg)
	if opts.Addr != "redis.example.com:6380" || opts.Username != cfg.Username || opts.Password != cfg.Password || opts.DB != 3 {
		t.Fatalf("buildSetupRedisOptions() did not preserve Redis fields")
	}
	if opts.TLSConfig == nil || opts.TLSConfig.ServerName != cfg.Host {
		t.Fatalf("buildSetupRedisOptions() TLS config = %#v", opts.TLSConfig)
	}
	if opts.DialTimeout != 0 || opts.ReadTimeout != 0 || opts.WriteTimeout != 0 {
		t.Fatalf("setup options unexpectedly changed go-redis timeout defaults: %#v", opts)
	}
}

func TestSetupConfigFromEnvPreservesRedisUsername(t *testing.T) {
	t.Setenv("REDIS_USERNAME", " acl:user/@% ")
	t.Setenv("REDIS_PASSWORD", " password/@% ")

	cfg := setupConfigFromEnv()
	if cfg.Redis.Username != " acl:user/@% " || cfg.Redis.Password != " password/@% " {
		t.Fatalf("setupConfigFromEnv() did not preserve Redis credentials exactly")
	}
}

func TestValidateRedisUsernameUsesUTF8ByteLimit(t *testing.T) {
	if err := validateRedisUsername(""); err != nil {
		t.Fatalf("empty username should select Redis default user: %v", err)
	}
	if err := validateRedisUsername(strings.Repeat("é", 64)); err != nil {
		t.Fatalf("128-byte username should be valid: %v", err)
	}

	const secret = "secret-username/@%"
	err := validateRedisUsername(strings.Repeat("a", 127) + "é" + secret)
	if err == nil || !strings.Contains(err.Error(), "at most 128 bytes") {
		t.Fatalf("129+ byte username error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked username: %v", err)
	}
}

func TestRedisConnectionRejectsOversizeUsernameBeforeDial(t *testing.T) {
	username := strings.Repeat("a", 129)
	const password = "password-must-not-leak/@%"
	err := TestRedisConnection(&RedisConfig{
		Host: "127.0.0.1", Port: 1, Username: username, Password: password,
	})
	if err == nil || err.Error() != "Redis username must be at most 128 bytes" {
		t.Fatalf("TestRedisConnection() error = %v", err)
	}
	if strings.Contains(err.Error(), username) || strings.Contains(err.Error(), password) {
		t.Fatalf("validation error leaked Redis credentials: %v", err)
	}
}

func TestWriteConfigFileIncludesRedisUsernameExactly(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	configPath := GetConfigFilePath()
	requireNoError(t, os.WriteFile(configPath, []byte("old config"), 0o644))
	requireNoError(t, os.Chmod(configPath, 0o644))

	cfg := &SetupConfig{Redis: RedisConfig{
		Host: "redis", Port: 6379, Username: " acl:user/@% ", Password: " password/@% ", DB: 2,
	}}
	requireNoError(t, writeConfigFile(cfg))

	data, err := os.ReadFile(configPath)
	requireNoError(t, err)
	var written SetupConfig
	requireNoError(t, yaml.Unmarshal(data, &written))
	if written.Redis.Username != cfg.Redis.Username || written.Redis.Password != cfg.Redis.Password {
		t.Fatalf("written Redis credentials changed during YAML serialization")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		requireNoError(t, err)
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config permissions = %o, want 600", got)
		}
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDecideAdminBootstrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		totalUsers int64
		adminUsers int64
		should     bool
		reason     string
	}{
		{
			name:       "empty database should create admin",
			totalUsers: 0,
			adminUsers: 0,
			should:     true,
			reason:     adminBootstrapReasonEmptyDatabase,
		},
		{
			name:       "admin exists should skip",
			totalUsers: 10,
			adminUsers: 1,
			should:     false,
			reason:     adminBootstrapReasonAdminExists,
		},
		{
			name:       "users exist without admin should skip",
			totalUsers: 5,
			adminUsers: 0,
			should:     false,
			reason:     adminBootstrapReasonUsersExistWithoutAdmin,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideAdminBootstrap(tc.totalUsers, tc.adminUsers)
			if got.shouldCreate != tc.should {
				t.Fatalf("shouldCreate=%v, want %v", got.shouldCreate, tc.should)
			}
			if got.reason != tc.reason {
				t.Fatalf("reason=%q, want %q", got.reason, tc.reason)
			}
		})
	}
}

func TestSetupDefaultAdminConcurrency(t *testing.T) {
	t.Run("simple mode admin uses higher concurrency", func(t *testing.T) {
		t.Setenv("RUN_MODE", "simple")
		if got := setupDefaultAdminConcurrency(); got != simpleModeAdminConcurrency {
			t.Fatalf("setupDefaultAdminConcurrency()=%d, want %d", got, simpleModeAdminConcurrency)
		}
	})

	t.Run("standard mode keeps existing default", func(t *testing.T) {
		t.Setenv("RUN_MODE", "standard")
		if got := setupDefaultAdminConcurrency(); got != defaultUserConcurrency {
			t.Fatalf("setupDefaultAdminConcurrency()=%d, want %d", got, defaultUserConcurrency)
		}
	})
}

func TestWriteConfigFileKeepsDefaultUserConcurrency(t *testing.T) {
	t.Setenv("RUN_MODE", "simple")
	t.Setenv("DATA_DIR", t.TempDir())

	if err := writeConfigFile(&SetupConfig{}); err != nil {
		t.Fatalf("writeConfigFile() error = %v", err)
	}

	data, err := os.ReadFile(GetConfigFilePath())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if !strings.Contains(string(data), "user_concurrency: 5") {
		t.Fatalf("config missing default user concurrency, got:\n%s", string(data))
	}
}

func TestBuildDatabaseConnectionDSNsUsesPostgresForBootstrap(t *testing.T) {
	cfg := &DatabaseConfig{
		Host:     "db",
		Port:     5432,
		User:     "sub2api",
		Password: "secret",
		DBName:   "sub2api",
		SSLMode:  "disable",
	}

	bootstrapDSN, targetDSN := buildDatabaseConnectionDSNs(cfg)

	if !strings.Contains(bootstrapDSN, "dbname=postgres") {
		t.Fatalf("bootstrap DSN = %q, want default postgres database", bootstrapDSN)
	}
	if strings.Contains(bootstrapDSN, "dbname=sub2api") {
		t.Fatalf("bootstrap DSN = %q, should not connect to target database before checking/creating it", bootstrapDSN)
	}
	if !strings.Contains(targetDSN, "dbname=sub2api") {
		t.Fatalf("target DSN = %q, want configured database", targetDSN)
	}
}

func TestBuildDatabaseConnectionDSNsArePgxCompatible(t *testing.T) {
	cfg := &DatabaseConfig{
		Host:     "db",
		Port:     5432,
		User:     "sub2api",
		Password: "secret",
		DBName:   "sub2api",
		SSLMode:  "disable",
	}

	for name, dsn := range map[string]string{
		"bootstrap": buildPostgresDSN(cfg, "postgres"),
		"target":    buildPostgresDSN(cfg, cfg.DBName),
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("pgx.ParseConfig(%q) error = %v", dsn, err)
			}
			if parsed.User != cfg.User {
				t.Fatalf("parsed user = %q, want %q", parsed.User, cfg.User)
			}
		})
	}
}
