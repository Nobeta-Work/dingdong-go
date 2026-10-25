package trade

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type activityCache struct {
	service *service
	group   singleflight.Group
	budget  chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

func newActivityCache(s *service) *activityCache {
	ctx, cancel := context.WithCancel(context.Background())
	return &activityCache{service: s, budget: make(chan struct{}, s.config.CacheOrigins), ctx: ctx, cancel: cancel}
}

func detailKey(id string) string { return "dingdong:activity-detail:" + id }

func (cache *activityCache) read(ctx context.Context, id string) (activity, bool, error) {
	redisCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	body, err := cache.service.redis.Get(redisCtx, detailKey(id)).Bytes()
	if err != nil {
		return activity{}, false, nil
	}
	if string(body) == "null" {
		return activity{}, true, sql.ErrNoRows
	}
	var a activity
	if json.Unmarshal(body, &a) != nil {
		return activity{}, false, nil
	}
	activityPhase(&a)
	return a, true, nil
}

// get 的共享加载使用服务上下文：一个等待者断开不会取消其他等待者的回源。
// singleflight 只合并相同 key；budget 则限制不同 key 的总数据库并发。
func (cache *activityCache) get(ctx context.Context, id string) (activity, error) {
	if a, found, err := cache.read(ctx, id); found {
		cache.service.metrics.Add("cache.hit", 1)
		return a, err
	}
	cache.service.metrics.Add("cache.miss", 1)
	result := cache.group.DoChan(id, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(cache.ctx, 2*time.Second)
		defer cancel()
		if a, found, err := cache.read(loadCtx, id); found {
			return a, err
		}
		select {
		case cache.budget <- struct{}{}:
			defer func() { <-cache.budget }()
		default:
			cache.service.metrics.Add("cache.origin_rejected", 1)
			return activity{}, status.Error(codes.Unavailable, "活动回源并发预算已满")
		}
		cache.service.metrics.Add("cache.db_load", 1)
		a, err := getActivity(loadCtx, cache.service.db, id, false)
		if err == nil && a.Status != "PUBLISHED" && a.Status != "PAUSED" {
			err = sql.ErrNoRows
		}
		if err != nil && err != sql.ErrNoRows {
			return a, err
		}
		body := []byte("null")
		ttl := 5 * time.Second
		if err == nil {
			body, err = json.Marshal(a)
			if err != nil {
				return a, err
			}
			ttl = 60*time.Second + time.Duration(rand.IntN(16))*time.Second
		}
		redisCtx, redisCancel := context.WithTimeout(loadCtx, 300*time.Millisecond)
		setErr := cache.service.redis.Set(redisCtx, detailKey(id), string(body), ttl).Err()
		redisCancel()
		if setErr != nil {
			cache.service.metrics.Add("cache.write_failed", 1)
		}
		if string(body) == "null" {
			return activity{}, sql.ErrNoRows
		}
		return a, nil
	})
	select {
	case <-ctx.Done():
		return activity{}, ctx.Err()
	case loaded := <-result:
		if loaded.Shared {
			cache.service.metrics.Add("cache.shared_wait", 1)
		}
		if loaded.Err != nil {
			return activity{}, loaded.Err
		}
		a := loaded.Val.(activity)
		activityPhase(&a)
		return a, nil
	}
}

func (s *service) invalidateTask(ctx context.Context, tx *sql.Tx, id string) error {
	return work.PutTask(ctx, tx, uuid.NewString(), "InvalidateActivityCache", map[string]string{"activityId": id}, time.Now(), "cache-invalidation")
}

func (s *service) invalidate(ctx context.Context, id string) error {
	err := s.redis.Del(ctx, detailKey(id)).Err()
	if err == redis.Nil {
		return nil
	}
	return err
}
