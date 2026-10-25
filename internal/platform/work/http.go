package work

import (
	"strconv"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// Register 为当前队列提供运营查看和人工恢复，不能通过 URL 选择任意 SQL 表。
func (q *Queue) Register(router *gin.Engine, secret string, demoReplay bool) {
	g := router.Group("/api/admin/reliability/"+q.Table, auth.Require(secret, true))
	g.GET("", func(c *gin.Context) {
		page, size, err := httpx.Page(c)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		where := " WHERE 1=1"
		args := []any{}
		if state := c.Query("status"); state != "" {
			where += " AND status=?"
			args = append(args, state)
		}
		keyColumn := "event_id"
		if q.Table == "outbox" {
			keyColumn = "aggregate_id"
		}
		if q.Table == "operation_task" {
			keyColumn = "business_key"
		}
		if key := c.Query("key"); key != "" {
			where += " AND " + keyColumn + "=?"
			args = append(args, key)
		}
		var total int
		if err = q.DB.QueryRowContext(c.Request.Context(), "SELECT COUNT(*) FROM "+q.Table+where, args...).Scan(&total); err != nil {
			httpx.Error(c, err)
			return
		}
		args = append(args, size, (page-1)*size)
		extra := ""
		if q.Table == "event_inbox" {
			extra = ",deliveries"
		}
		rows, err := q.DB.QueryContext(c.Request.Context(), "SELECT id,payload,attempts,status,next_attempt_at,last_error"+extra+" FROM "+q.Table+where+" ORDER BY id LIMIT ? OFFSET ?", args...)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		defer rows.Close()
		items := make([]gin.H, 0, size)
		for rows.Next() {
			var id uint64
			var payload, state, message string
			var attempts int
			var next time.Time
			var deliveries int
			dest := []any{&id, &payload, &attempts, &state, &next, &message}
			if q.Table == "event_inbox" {
				dest = append(dest, &deliveries)
			}
			if err = rows.Scan(dest...); err != nil {
				httpx.Error(c, err)
				return
			}
			item := gin.H{"id": strconv.FormatUint(id, 10), "payload": payload, "attempts": attempts, "status": state, "nextAttemptAt": next, "lastError": message}
			if q.Table == "event_inbox" {
				item["deliveries"] = deliveries
			}
			items = append(items, item)
		}
		if err = rows.Err(); err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "队列记录", gin.H{"items": items, "total": total, "page": page, "pageSize": size})
	})
	g.GET("/stats", func(c *gin.Context) {
		rows, err := q.DB.QueryContext(c.Request.Context(), "SELECT status,COUNT(*) FROM "+q.Table+" GROUP BY status")
		if err != nil {
			httpx.Error(c, err)
			return
		}
		defer rows.Close()
		counts := map[string]int64{}
		for rows.Next() {
			var state string
			var n int64
			if err = rows.Scan(&state, &n); err != nil {
				httpx.Error(c, err)
				return
			}
			counts[state] = n
		}
		if err = rows.Err(); err != nil {
			httpx.Error(c, err)
			return
		}
		var due, age int64
		err = q.DB.QueryRowContext(c.Request.Context(), "SELECT COUNT(*),GREATEST(COALESCE(MAX(TIMESTAMPDIFF(SECOND,next_attempt_at,UTC_TIMESTAMP(3))),0),0) FROM "+q.Table+" WHERE status IN ('PENDING','PROCESSING') AND next_attempt_at<=UTC_TIMESTAMP(3)").Scan(&due, &age)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "队列统计", gin.H{"counts": counts, "dueCount": due, "oldestDueAgeSeconds": age})
	})
	g.POST("/:id/retry", func(c *gin.Context) { q.resume(c, false) })
	if demoReplay && q.Table == "outbox" {
		g.POST("/:id/replay", func(c *gin.Context) { q.resume(c, true) })
	}
}

func (q *Queue) resume(c *gin.Context, replay bool) {
	if err := httpx.ID(c.Param("id")); err != nil {
		httpx.Error(c, err)
		return
	}
	from := "NEEDS_REVIEW"
	if replay {
		from = "SENT"
	}
	result, err := q.DB.ExecContext(c.Request.Context(), "UPDATE "+q.Table+" SET status='PENDING',attempts=0,next_attempt_at=UTC_TIMESTAMP(3),lease_owner=NULL,lease_until=NULL,last_error='' WHERE id=? AND status=?", c.Param("id"), from)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if n != 1 {
		httpx.Write(c, 409, "CONFLICT", "记录不存在或当前状态不允许恢复", nil)
		return
	}
	httpx.Write(c, 200, "OK", "已恢复待处理", nil)
}
