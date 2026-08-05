package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	supportDecisionActiveKey         = "sched:support-decision:active"
	supportDecisionDocumentKeyPrefix = "sched:support-decision:document:"
	supportDecisionWakeupChannel     = "sched:support-decision:wakeup"

	maxSupportDecisionDocumentTTL = 7 * 24 * time.Hour
)

var supportDecisionActivateScript = redis.NewScript(`
local candidate = ARGV[1]

local function canonical(value)
    if value == nil or string.match(value, "^[1-9][0-9]*$") == nil then
        return false
    end
    -- Canonical publication generations are nonzero uint64 decimals.
    return #value < 20 or (#value == 20 and value <= "18446744073709551615")
end

if not canonical(candidate) then
    return redis.error_reply("invalid candidate generation")
end

local active = redis.call("GET", KEYS[1])
if active and not canonical(active) then
    return redis.error_reply("malformed active generation")
end

-- Compare canonical decimals as strings: Lua numbers lose uint64 precision.
if active and (#active > #candidate or (#active == #candidate and active >= candidate)) then
    return 0
end

if redis.call("EXISTS", KEYS[2]) == 0 then
    return redis.error_reply("support decision document not found")
end

redis.call("SET", KEYS[1], candidate)
return 1
`)

type supportDecisionRedis struct {
	rdb *redis.Client
}

func NewSupportDecisionPublicationStore(rdb *redis.Client) service.SupportDecisionPublicationStore {
	return newSupportDecisionRedis(rdb)
}

func newSupportDecisionRedis(rdb *redis.Client) *supportDecisionRedis {
	return &supportDecisionRedis{rdb: rdb}
}

func supportDecisionDocumentKey(generation uint64) string {
	return supportDecisionDocumentKeyPrefix + strconv.FormatUint(generation, 10)
}

func (s *supportDecisionRedis) PutDocument(ctx context.Context, generation uint64, payload []byte, ttl time.Duration) error {
	if generation == 0 {
		return errors.New("support decision generation must be nonzero")
	}
	if len(payload) == 0 {
		return errors.New("support decision payload must be nonempty")
	}
	if ttl < time.Millisecond || ttl > maxSupportDecisionDocumentTTL || ttl%time.Millisecond != 0 {
		return fmt.Errorf("support decision document TTL must be a whole millisecond within [%s, %s]", time.Millisecond, maxSupportDecisionDocumentTTL)
	}
	return s.rdb.Set(ctx, supportDecisionDocumentKey(generation), payload, ttl).Err()
}

func (s *supportDecisionRedis) Activate(ctx context.Context, generation uint64) (bool, error) {
	if generation == 0 {
		return false, errors.New("support decision generation must be nonzero")
	}
	generationText := strconv.FormatUint(generation, 10)
	result, err := supportDecisionActivateScript.Run(
		ctx,
		s.rdb,
		[]string{supportDecisionActiveKey, supportDecisionDocumentKey(generation)},
		generationText,
	).Int64()
	if err != nil {
		if err.Error() == "support decision document not found" {
			return false, fmt.Errorf("activate support decision generation %s: %w", generationText, service.ErrSupportDecisionDocumentNotFound)
		}
		// The script may have committed before a transport error was observed; never clear active state here.
		return false, fmt.Errorf("activate support decision generation %s: %w", generationText, err)
	}
	if result != 0 && result != 1 {
		return false, fmt.Errorf("activate support decision generation %s: unexpected Redis result %d", generationText, result)
	}
	return result == 1, nil
}

func (s *supportDecisionRedis) ActiveGeneration(ctx context.Context) (uint64, error) {
	value, err := s.rdb.Get(ctx, supportDecisionActiveKey).Result()
	if errors.Is(err, redis.Nil) {
		return 0, service.ErrSupportDecisionActiveGenerationNotFound
	}
	if err != nil {
		return 0, err
	}
	generation, err := parseCanonicalSupportDecisionGeneration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid support decision active generation: %w", err)
	}
	return generation, nil
}

func (s *supportDecisionRedis) GetDocument(ctx context.Context, generation uint64) ([]byte, error) {
	if generation == 0 {
		return nil, errors.New("support decision generation must be nonzero")
	}
	payload, err := s.rdb.Get(ctx, supportDecisionDocumentKey(generation)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, service.ErrSupportDecisionDocumentNotFound
	}
	return payload, err
}

func (s *supportDecisionRedis) PublishWakeup(ctx context.Context, generation uint64) error {
	if generation == 0 {
		return errors.New("support decision generation must be nonzero")
	}
	return s.rdb.Publish(ctx, supportDecisionWakeupChannel, strconv.FormatUint(generation, 10)).Err()
}

func (s *supportDecisionRedis) SubscribeWakeups(ctx context.Context) (service.SupportDecisionWakeupSubscription, error) {
	pubsub := s.rdb.Subscribe(ctx, supportDecisionWakeupChannel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("subscribe to support decision wakeups: %w", err)
	}
	return &supportDecisionWakeupSubscription{
		pubsub:   pubsub,
		messages: pubsub.Channel(),
	}, nil
}

type supportDecisionWakeupSubscription struct {
	pubsub    *redis.PubSub
	messages  <-chan *redis.Message
	closeOnce sync.Once
	closeErr  error
}

func (s *supportDecisionWakeupSubscription) Receive(ctx context.Context) (uint64, error) {
	var message *redis.Message
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case received, ok := <-s.messages:
		if !ok {
			return 0, errors.New("support decision wakeup subscription closed")
		}
		message = received
	}
	if message == nil {
		return 0, errors.New("support decision wakeup subscription returned a nil message")
	}
	generation, err := parseCanonicalSupportDecisionGeneration(message.Payload)
	if err != nil {
		return 0, fmt.Errorf("invalid support decision wakeup generation: %w", err)
	}
	return generation, nil
}

func (s *supportDecisionWakeupSubscription) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.pubsub.Close()
	})
	return s.closeErr
}

func parseCanonicalSupportDecisionGeneration(value string) (uint64, error) {
	if value == "" || value[0] == '0' {
		return 0, fmt.Errorf("noncanonical generation %q", value)
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("noncanonical generation %q", value)
		}
	}
	generation, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("generation %q is outside uint64: %w", value, err)
	}
	if generation == 0 || strconv.FormatUint(generation, 10) != value {
		return 0, fmt.Errorf("noncanonical generation %q", value)
	}
	return generation, nil
}
