package repository

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/setting"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"golang.org/x/sync/singleflight"
)

// settingCacheTTL settings 表进程内缓存 TTL。settings 变化极少（管理后台写），
// 写路径（Set/SetMultiple/Delete）会显式失效对应键与全量快照；TTL 只是针对
// 任何遗漏写入路径的安全网，30s 内自动恢复一致。
const settingCacheTTL = 30 * time.Second

type settingCacheEntry struct {
	setting   *service.Setting // nil 表示负缓存（键不存在）
	found     bool
	expiresAt int64 // unix nano
}

type settingAllCacheEntry struct {
	values    map[string]string
	expiresAt int64 // unix nano
}

type settingRepository struct {
	client *ent.Client
	mu     sync.Mutex
	// keyCache 键级缓存：Get/GetValue/GetMultiple 共享，写路径按键失效。
	keyCache map[string]settingCacheEntry
	// allCache 全量快照：GetAll 使用，任何写路径整体失效。
	allCache *settingAllCacheEntry
	// sf 防止缓存过期时多个请求同时回源（thundering herd）。
	sf singleflight.Group
}

func NewSettingRepository(client *ent.Client) service.SettingRepository {
	return &settingRepository{client: client, keyCache: make(map[string]settingCacheEntry)}
}

func (r *settingRepository) peekKeyCache(key string) (settingCacheEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.keyCache[key]
	if !ok || time.Now().UnixNano() >= entry.expiresAt {
		return settingCacheEntry{}, false
	}
	return entry, true
}

func (r *settingRepository) storeKeyCache(key string, entry settingCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keyCache == nil {
		r.keyCache = make(map[string]settingCacheEntry)
	}
	r.keyCache[key] = entry
}

func (r *settingRepository) invalidateKeyCache(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.keyCache, key)
}

func (r *settingRepository) invalidateAllCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allCache = nil
}

// loadSetting 直接查库；键不存在时返回 (nil, false, nil)，便于负缓存。
func (r *settingRepository) loadSetting(ctx context.Context, key string) (*service.Setting, bool, error) {
	m, err := r.client.Setting.Query().Where(setting.KeyEQ(key)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &service.Setting{
		ID:        m.ID,
		Key:       m.Key,
		Value:     m.Value,
		UpdatedAt: m.UpdatedAt,
	}, true, nil
}

func (r *settingRepository) Get(ctx context.Context, key string) (*service.Setting, error) {
	if entry, ok := r.peekKeyCache(key); ok {
		if !entry.found {
			return nil, service.ErrSettingNotFound
		}
		setting := *entry.setting // 浅拷贝：调用方不持有内部指针，防误改缓存。
		return &setting, nil
	}
	value, err, _ := r.sf.Do("setting:"+key, func() (any, error) {
		if entry, ok := r.peekKeyCache(key); ok {
			return entry, nil
		}
		setting, found, err := r.loadSetting(ctx, key)
		if err != nil {
			return nil, err
		}
		entry := settingCacheEntry{
			setting:   setting,
			found:     found,
			expiresAt: time.Now().Add(settingCacheTTL).UnixNano(),
		}
		r.storeKeyCache(key, entry)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	entry, ok := value.(settingCacheEntry)
	if !ok || !entry.found {
		return nil, service.ErrSettingNotFound
	}
	setting := *entry.setting
	return &setting, nil
}

func (r *settingRepository) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *settingRepository) Set(ctx context.Context, key, value string) error {
	now := time.Now()
	err := r.client.Setting.
		Create().
		SetKey(key).
		SetValue(value).
		SetUpdatedAt(now).
		OnConflictColumns(setting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
	if err == nil {
		r.invalidateKeyCache(key)
		r.invalidateAllCache()
	}
	return err
}

func (r *settingRepository) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	result := make(map[string]string, len(keys))
	var missing []string
	for _, key := range keys {
		if entry, ok := r.peekKeyCache(key); ok {
			if entry.found {
				result[key] = entry.setting.Value
			}
			continue
		}
		missing = append(missing, key)
	}
	if len(missing) == 0 {
		return result, nil
	}

	settings, err := r.client.Setting.Query().Where(setting.KeyIn(missing...)).All(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Add(settingCacheTTL).UnixNano()
	seen := make(map[string]struct{}, len(settings))
	for _, m := range settings {
		result[m.Key] = m.Value
		seen[m.Key] = struct{}{}
		r.storeKeyCache(m.Key, settingCacheEntry{
			setting:   &service.Setting{ID: m.ID, Key: m.Key, Value: m.Value, UpdatedAt: m.UpdatedAt},
			found:     true,
			expiresAt: now,
		})
	}
	// 请求了但查询未返回的键：负缓存（键不存在），后续读取零 DB 往返。
	for _, key := range missing {
		if _, ok := seen[key]; !ok {
			r.storeKeyCache(key, settingCacheEntry{found: false, expiresAt: now})
		}
	}
	return result, nil
}

func (r *settingRepository) SetMultiple(ctx context.Context, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}

	now := time.Now()
	builders := make([]*ent.SettingCreate, 0, len(settings))
	for key, value := range settings {
		builders = append(builders, r.client.Setting.Create().SetKey(key).SetValue(value).SetUpdatedAt(now))
	}
	err := r.client.Setting.
		CreateBulk(builders...).
		OnConflictColumns(setting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
	if err == nil {
		for key := range settings {
			r.invalidateKeyCache(key)
		}
		r.invalidateAllCache()
	}
	return err
}

func (r *settingRepository) GetAll(ctx context.Context) (map[string]string, error) {
	r.mu.Lock()
	if r.allCache != nil && time.Now().UnixNano() < r.allCache.expiresAt {
		values := make(map[string]string, len(r.allCache.values))
		for key, value := range r.allCache.values {
			values[key] = value
		}
		r.mu.Unlock()
		return values, nil
	}
	r.mu.Unlock()

	value, err, _ := r.sf.Do("setting:all", func() (any, error) {
		r.mu.Lock()
		if r.allCache != nil && time.Now().UnixNano() < r.allCache.expiresAt {
			values := make(map[string]string, len(r.allCache.values))
			for key, value := range r.allCache.values {
				values[key] = value
			}
			r.mu.Unlock()
			return values, nil
		}
		r.mu.Unlock()

		settings, err := r.client.Setting.Query().All(ctx)
		if err != nil {
			return nil, err
		}
		values := make(map[string]string, len(settings))
		for _, s := range settings {
			values[s.Key] = s.Value
		}
		r.mu.Lock()
		r.allCache = &settingAllCacheEntry{values: values, expiresAt: time.Now().Add(settingCacheTTL).UnixNano()}
		r.mu.Unlock()
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	values, ok := value.(map[string]string)
	if !ok {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result, nil
}

func (r *settingRepository) Delete(ctx context.Context, key string) error {
	_, err := r.client.Setting.Delete().Where(setting.KeyEQ(key)).Exec(ctx)
	if err == nil {
		r.invalidateKeyCache(key)
		r.invalidateAllCache()
	}
	return err
}
