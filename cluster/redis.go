package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var ErrSessionOwned = errors.New("session is owned by another instance")

type RedisRuntime struct {
	client    redis.UniversalClient
	clusterID string
	claimIdle time.Duration
	closeOnce sync.Once
}

func (r *RedisRuntime) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("Redis runtime is unavailable")
	}
	return r.client.Ping(ctx).Err()
}

func NewRedisRuntime(ctx context.Context, redisURL, clusterID string) (*RedisRuntime, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, errors.New("invalid redis_url")
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect redis: %w", err)
	}
	if strings.TrimSpace(clusterID) == "" {
		clusterID = "default"
	}
	return &RedisRuntime{client: client, clusterID: clusterID, claimIdle: 15 * time.Second}, nil
}

func NewRedisRuntimeWithClient(client redis.UniversalClient, clusterID string) *RedisRuntime {
	if strings.TrimSpace(clusterID) == "" {
		clusterID = "default"
	}
	return &RedisRuntime{client: client, clusterID: clusterID, claimIdle: 15 * time.Second}
}

func (r *RedisRuntime) SetCommandClaimIdle(value time.Duration) {
	if value > 0 {
		r.claimIdle = value
	}
}

func (r *RedisRuntime) RegisterInstance(ctx context.Context, info InstanceInfo, ttl time.Duration) error {
	if strings.TrimSpace(info.InstanceID) == "" {
		return errors.New("instance ID is required")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.instanceKey(info.InstanceID), data, ttl).Err()
}

func (r *RedisRuntime) UnregisterInstance(ctx context.Context, instanceID string) error {
	return r.client.Del(ctx, r.instanceKey(instanceID)).Err()
}

func (r *RedisRuntime) Instance(ctx context.Context, instanceID string) (*InstanceInfo, error) {
	value, err := r.client.Get(ctx, r.instanceKey(instanceID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info InstanceInfo
	if err := json.Unmarshal(value, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (r *RedisRuntime) Lookup(ctx context.Context, tenantID, sessionID string) (*Owner, error) {
	value, err := r.client.Get(ctx, r.ownerKey(tenantID, sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var owner Owner
	if err := json.Unmarshal([]byte(value), &owner); err != nil {
		return nil, err
	}
	return &owner, nil
}

var acquireLeaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return {0, redis.call('GET', KEYS[1])}
end
local fence = redis.call('INCR', KEYS[2])
local owner = cjson.encode({instance_id=ARGV[1], lease_id=ARGV[2], fence_token=fence})
local written = redis.call('SET', KEYS[1], owner, 'PX', ARGV[3], 'NX')
if not written then
  return {0, redis.call('GET', KEYS[1])}
end
return {fence, owner}
`)

var renewLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
return redis.call('PEXPIRE', KEYS[1], ARGV[2])
`)

var releaseLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
return redis.call('DEL', KEYS[1])
`)

func (r *RedisRuntime) Acquire(ctx context.Context, tenantID, sessionID, instanceID string,
	ttl, renewEvery time.Duration) (SessionLease, error) {
	if ttl <= 0 || renewEvery <= 0 || renewEvery >= ttl {
		return nil, errors.New("invalid lease timing")
	}
	leaseID := uuid.NewString()
	result, err := acquireLeaseScript.Run(ctx, r.client,
		[]string{r.ownerKey(tenantID, sessionID), r.fenceKey(tenantID, sessionID)},
		instanceID, leaseID, ttl.Milliseconds()).Slice()
	if err != nil {
		return nil, err
	}
	fence, err := redisInt64(result[0])
	if err != nil {
		return nil, err
	}
	value := fmt.Sprint(result[1])
	var owner Owner
	if err := json.Unmarshal([]byte(value), &owner); err != nil {
		return nil, err
	}
	if fence == 0 || owner.InstanceID != instanceID || owner.LeaseID != leaseID {
		return nil, fmt.Errorf("%w: %s", ErrSessionOwned, value)
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	lease := &redisLease{runtime: r, tenantID: tenantID, sessionID: sessionID, owner: owner,
		ttl: ttl, renewEvery: renewEvery, value: value, lost: make(chan struct{}), cancel: cancel,
		lastRenewed: time.Now()}
	go lease.renewLoop(leaseCtx)
	return lease, nil
}

type redisLease struct {
	runtime     *RedisRuntime
	tenantID    string
	sessionID   string
	owner       Owner
	ttl         time.Duration
	renewEvery  time.Duration
	value       string
	lost        chan struct{}
	lostOnce    sync.Once
	releaseOnce sync.Once
	cancel      context.CancelFunc
	lastRenewed time.Time
}

func (l *redisLease) Owner() Owner          { return l.owner }
func (l *redisLease) Lost() <-chan struct{} { return l.lost }

func (l *redisLease) renewLoop(ctx context.Context) {
	ticker := time.NewTicker(l.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			result, err := renewLeaseScript.Run(ctx, l.runtime.client,
				[]string{l.runtime.ownerKey(l.tenantID, l.sessionID)}, l.value, l.ttl.Milliseconds()).Int64()
			if err == nil && result == 1 {
				l.lastRenewed = now
				continue
			}
			if err == nil || now.Add(l.renewEvery).After(l.lastRenewed.Add(l.ttl)) {
				l.markLost()
				return
			}
		}
	}
}

func (l *redisLease) Release(ctx context.Context) error {
	var err error
	l.releaseOnce.Do(func() {
		l.cancel()
		_, err = releaseLeaseScript.Run(ctx, l.runtime.client,
			[]string{l.runtime.ownerKey(l.tenantID, l.sessionID)}, l.value).Result()
		l.markLost()
	})
	return err
}

func (l *redisLease) markLost() {
	l.lostOnce.Do(func() { close(l.lost) })
}

func (r *RedisRuntime) Forward(ctx context.Context, command CommandEnvelope) (CommandResponse, error) {
	if command.RequestID == "" || command.OwnerInstanceID == "" || command.GatewayInstanceID == "" {
		return CommandResponse{}, errors.New("incomplete cluster command")
	}
	data, err := json.Marshal(command)
	if err != nil {
		return CommandResponse{}, err
	}
	commandKey := r.commandKey(command.OwnerInstanceID)
	if _, err := r.client.XAdd(ctx, &redis.XAddArgs{Stream: commandKey, MaxLen: 100000, Approx: true,
		Values: map[string]any{"payload": data}}).Result(); err != nil {
		return CommandResponse{}, err
	}
	responseKey := r.responseKey(command.GatewayInstanceID, command.RequestID)
	timeout := time.Until(command.Deadline)
	if timeout <= 0 {
		return CommandResponse{}, context.DeadlineExceeded
	}
	streams, err := r.client.XRead(ctx, &redis.XReadArgs{Streams: []string{responseKey, "0-0"},
		Count: 1, Block: timeout}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return CommandResponse{}, context.DeadlineExceeded
		}
		return CommandResponse{}, err
	}
	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return CommandResponse{}, context.DeadlineExceeded
	}
	payload := fmt.Sprint(streams[0].Messages[0].Values["payload"])
	var response CommandResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		return CommandResponse{}, err
	}
	return response, nil
}

func (r *RedisRuntime) Read(ctx context.Context, instanceID, consumer string, count int64,
	block time.Duration) ([]CommandEnvelope, error) {
	key := r.commandKey(instanceID)
	group := "owner"
	if err := r.client.XGroupCreateMkStream(ctx, key, group, "0-0").Err(); err != nil &&
		!strings.Contains(err.Error(), "BUSYGROUP") {
		return nil, err
	}
	claimed, _, claimErr := r.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: key, Group: group,
		Consumer: consumer, MinIdle: r.claimIdle, Start: "0-0", Count: count}).Result()
	if claimErr != nil && !errors.Is(claimErr, redis.Nil) {
		return nil, claimErr
	}
	result, err := r.decodeCommands(ctx, key, group, claimed)
	if err != nil || len(result) > 0 {
		return result, err
	}
	streams, err := r.client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: group, Consumer: consumer,
		Streams: []string{key, ">"}, Count: count, Block: block}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var messages []redis.XMessage
	for _, stream := range streams {
		messages = append(messages, stream.Messages...)
	}
	return r.decodeCommands(ctx, key, group, messages)
}

func (r *RedisRuntime) decodeCommands(ctx context.Context, key, group string,
	messages []redis.XMessage) ([]CommandEnvelope, error) {
	result := make([]CommandEnvelope, 0, len(messages))
	for _, message := range messages {
		var command CommandEnvelope
		if err := json.Unmarshal([]byte(fmt.Sprint(message.Values["payload"])), &command); err != nil {
			// Poison messages cannot become executable after retry. Acknowledge
			// only the malformed envelope so valid pending commands remain intact.
			_ = r.client.XAck(ctx, key, group, message.ID).Err()
			continue
		}
		command.StreamID = message.ID
		result = append(result, command)
	}
	return result, nil
}

func (r *RedisRuntime) Respond(ctx context.Context, gatewayInstanceID string,
	response CommandResponse) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	key := r.responseKey(gatewayInstanceID, response.RequestID)
	pipe := r.client.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: key, MaxLen: 1, Values: map[string]any{"payload": data}})
	pipe.Expire(ctx, key, 24*time.Hour)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisRuntime) Ack(ctx context.Context, instanceID, streamID string) error {
	return r.client.XAck(ctx, r.commandKey(instanceID), "owner", streamID).Err()
}

func (r *RedisRuntime) Publish(ctx context.Context, notification EventNotification) error {
	data, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, r.notifyKey(notification.TenantID, notification.SessionID), data).Err()
}

func (r *RedisRuntime) Subscribe(ctx context.Context, tenantID, sessionID string) (<-chan EventNotification, func(), error) {
	pubsub := r.client.Subscribe(ctx, r.notifyKey(tenantID, sessionID))
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, err
	}
	out := make(chan EventNotification, 32)
	stop := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-stop:
				return
			case message, ok := <-pubsub.Channel():
				if !ok {
					return
				}
				var notification EventNotification
				if json.Unmarshal([]byte(message.Payload), &notification) == nil {
					select {
					case out <- notification:
					case <-stop:
						return
					}
				}
			}
		}
	}()
	var once sync.Once
	cancel := func() { once.Do(func() { close(stop); _ = pubsub.Close() }) }
	return out, cancel, nil
}

func (r *RedisRuntime) Close() error {
	var err error
	r.closeOnce.Do(func() { err = r.client.Close() })
	return err
}

func (r *RedisRuntime) instanceKey(instanceID string) string {
	return fmt.Sprintf("lumina:%s:instance:%s", r.clusterID, instanceID)
}

func (r *RedisRuntime) sessionTag(tenantID, sessionID string) string {
	escape := func(value string) string {
		replacer := strings.NewReplacer("%", "%25", "{", "%7B", "}", "%7D", "|", "%7C", ":", "%3A")
		return replacer.Replace(value)
	}
	return escape(tenantID) + "|" + escape(sessionID)
}

func (r *RedisRuntime) ownerKey(tenantID, sessionID string) string {
	return fmt.Sprintf("lumina:%s:session:{%s}:owner", r.clusterID, r.sessionTag(tenantID, sessionID))
}

func (r *RedisRuntime) fenceKey(tenantID, sessionID string) string {
	return fmt.Sprintf("lumina:%s:session:{%s}:fence", r.clusterID, r.sessionTag(tenantID, sessionID))
}

func (r *RedisRuntime) commandKey(instanceID string) string {
	return fmt.Sprintf("lumina:%s:rpc:%s:commands", r.clusterID, instanceID)
}

func (r *RedisRuntime) responseKey(instanceID, requestID string) string {
	return fmt.Sprintf("lumina:%s:rpc:%s:responses:%s", r.clusterID, instanceID, requestID)
}

func (r *RedisRuntime) notifyKey(tenantID, sessionID string) string {
	return fmt.Sprintf("lumina:%s:notify:{%s}", r.clusterID, r.sessionTag(tenantID, sessionID))
}

func redisInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis integer %T", value)
	}
}

var _ SessionCoordinator = (*RedisRuntime)(nil)
var _ ClusterCommandBus = (*RedisRuntime)(nil)
var _ EventNotifier = (*RedisRuntime)(nil)
