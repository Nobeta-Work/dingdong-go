package trade

import (
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// requestLimit 先处理持久化重放，再分别扣用户桶和活动桶；后一桶拒绝不退还前一桶。
func (s *service) requestLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		var input struct {
			RequestID string `json:"requestId" binding:"required,max=64"`
		}
		if c.ShouldBindJSON(&input) != nil || !requestIDPattern.MatchString(input.RequestID) {
			c.Abort()
			httpx.Error(c, status.Error(codes.InvalidArgument, "requestId 不合法"))
			return
		}
		id, hash, err := requestParameters(c.Param("id"))
		if err != nil {
			c.Abort()
			httpx.Error(c, err)
			return
		}
		if r, exists, err := s.replay(c.Request.Context(), c.GetString("userId"), input.RequestID, hash); err != nil || exists {
			c.Abort()
			if err != nil {
				httpx.Error(c, err)
			} else {
				httpx.Write(c, 200, "OK", "原受理结果", r)
			}
			return
		}
		for _, bucket := range []struct {
			key, reason string
			burst, rate int
		}{
			{"dingdong:rate:user:" + c.GetString("userId") + ":requests", "USER_RATE_LIMITED", s.config.UserBurst, s.config.UserRate},
			{"dingdong:{activity:" + id + "}:rate", "ACTIVITY_RATE_LIMITED", s.config.ActivityBurst, s.config.ActivityRate},
		} {
			allowed, err := rateScript.Run(c.Request.Context(), s.redis, []string{bucket.key}, bucket.burst, bucket.rate).Int()
			if err != nil {
				c.Abort()
				httpx.Error(c, status.Error(codes.Unavailable, "限流依赖暂不可用"))
				return
			}
			if allowed != 1 {
				// 同键并发时另一请求可能刚提交，重新查询原结果，不把它改成限流终态。
				r, exists, replayErr := s.replay(c.Request.Context(), c.GetString("userId"), input.RequestID, hash)
				c.Abort()
				if replayErr != nil {
					httpx.Error(c, replayErr)
				} else if exists {
					httpx.Write(c, 200, "OK", "原受理结果", r)
				} else {
					s.metrics.Add("rate.rejected", 1)
					httpx.Error(c, status.Error(codes.ResourceExhausted, bucket.reason))
				}
				return
			}
		}
		c.Set("requestId", input.RequestID)
		c.Next()
	}
}
