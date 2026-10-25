package trade

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func configure(r *app.Runtime) (func(context.Context) error, error) {
	s := newService(r)
	s.routes(r)
	bus, err := event.Start(r.DB, r.Config.Nameserver, "dingdong-trade", r.Logger, s.handleEvent)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.recoverLoop(ctx, r.Logger) }()
	return func(stopCtx context.Context) error {
		cancel()
		// 同时停止 MQ 与 Outbox 领取，恢复任务的在途事务仍可完成并持久化后续事件。
		busErr := bus.Close(stopCtx)
		select {
		case <-done:
			return busErr
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}, nil
}

func (s *service) routes(r *app.Runtime) {
	r.Router.Use(func(c *gin.Context) {
		if id := c.Param("id"); id != "" {
			if err := httpx.ID(id); err != nil {
				c.Abort()
				httpx.Error(c, err)
				return
			}
		}
		c.Next()
	})
	r.Router.GET("/api/activities", s.listActivities)
	r.Router.GET("/api/activities/:id", func(c *gin.Context) {
		a, err := getActivity(c.Request.Context(), s.db, c.Param("id"), false)
		if err == nil && a.Status != "PUBLISHED" && a.Status != "PAUSED" {
			err = sql.ErrNoRows
		}
		respond(c, a, err)
	})
	user := r.Router.Group("/api", auth.Require(r.Config.JWTSecret, false))
	user.POST("/activities/:id/requests", func(c *gin.Context) {
		var in struct {
			RequestID string `json:"requestId" binding:"required,max=64"`
		}
		if c.ShouldBindJSON(&in) != nil {
			httpx.Error(c, status.Error(codes.InvalidArgument, "缺少合法 requestId"))
			return
		}
		result, created, err := s.submit(c.Request.Context(), c.Param("id"), c.GetString("userId"), in.RequestID, c.GetString("traceId"))
		if err != nil {
			httpx.Error(c, err)
			return
		}
		code := 200
		if created {
			code = 202
		}
		httpx.Write(c, code, "OK", "请查询受理结果，受理不保证成单", result)
	})
	user.GET("/requests/:requestId", func(c *gin.Context) {
		result, err := readRequest(c.Request.Context(), s.db, c.GetString("userId"), c.Param("requestId"))
		respond(c, result, err)
	})
	user.GET("/activities/:id/participation", s.participation)
	user.GET("/orders", func(c *gin.Context) { s.listOrders(c, false) })
	user.GET("/orders/:orderNo", func(c *gin.Context) {
		o, err := readOrder(c.Request.Context(), s.db, c.Param("orderNo"))
		if err == nil && o.UserID != c.GetString("userId") {
			err = status.Error(codes.PermissionDenied, "不能查询他人订单")
		}
		respond(c, o, err)
	})
	user.POST("/orders/:orderNo/payment", func(c *gin.Context) {
		p, err := s.createPayment(c.Request.Context(), c.Param("orderNo"), c.GetString("userId"))
		if err != nil {
			httpx.Error(c, err)
			return
		}
		respond(c, gin.H{"paymentNo": p.PaymentNo, "orderNo": p.OrderNo, "userId": p.UserId, "amountCent": p.AmountCent,
			"expiresAt": time.UnixMilli(p.ExpiresAtUnixMilli).UTC(), "status": strings.TrimPrefix(p.Status.String(), "PAYMENT_STATUS_"), "simulated": true}, nil)
	})
	admin := r.Router.Group("/api/admin", auth.Require(r.Config.JWTSecret, true))
	admin.POST("/activities", func(c *gin.Context) {
		var in createActivity
		if c.ShouldBindJSON(&in) != nil {
			httpx.Error(c, status.Error(codes.InvalidArgument, "活动字段不合法，时间须为带时区 ISO 8601"))
			return
		}
		a, err := s.createActivity(c.Request.Context(), in)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 201, "OK", "草稿已创建", a)
	})
	admin.POST("/activities/:id/publish", func(c *gin.Context) { a, err := s.publish(c.Request.Context(), c.Param("id")); respond(c, a, err) })
	admin.POST("/activities/:id/pause", func(c *gin.Context) { a, err := s.pause(c.Request.Context(), c.Param("id")); respond(c, a, err) })
	admin.GET("/activities/:id/stats", s.stats)
	admin.GET("/orders", func(c *gin.Context) { s.listOrders(c, true) })
}

func respond(c *gin.Context, data any, err error) {
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Write(c, 200, "OK", "操作成功", data)
}

func (s *service) listActivities(c *gin.Context) {
	page, size, err := httpx.Page(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var total int
	ctx := c.Request.Context()
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM activity WHERE status IN ('PUBLISHED','PAUSED')").Scan(&total)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+activityColumns+" FROM activity WHERE status IN ('PUBLISHED','PAUSED') ORDER BY id DESC LIMIT ? OFFSET ?", size, (page-1)*size)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	defer rows.Close()
	items := make([]activity, 0, size)
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		items = append(items, a)
	}
	if err = rows.Err(); err != nil {
		httpx.Error(c, err)
		return
	}
	respond(c, gin.H{"items": items, "total": total, "page": page, "pageSize": size}, nil)
}

func (s *service) participation(c *gin.Context) {
	a, err := getActivity(c.Request.Context(), s.db, c.Param("id"), false)
	if err == nil && a.Status != "PUBLISHED" && a.Status != "PAUSED" {
		err = sql.ErrNoRows
	}
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var holder sql.NullString
	var state string
	err = s.db.QueryRowContext(c.Request.Context(), "SELECT holder_request_id,status FROM activity_participation WHERE activity_id=? AND user_id=?", c.Param("id"), c.GetString("userId")).Scan(&holder, &state)
	if err == nil && !holder.Valid {
		// 失败释放资格后仍展示最近一次失败，方便客户端决定是否用新 requestId 重试。
		err = s.db.QueryRowContext(c.Request.Context(), "SELECT request_id FROM activity_request WHERE activity_id=? AND user_id=? ORDER BY id DESC LIMIT 1", a.ID, c.GetString("userId")).Scan(&holder.String)
	}
	if err == sql.ErrNoRows {
		respond(c, gin.H{"status": "AVAILABLE", "requestId": "", "orderNo": "", "failureReason": ""}, nil)
		return
	}
	if err != nil {
		httpx.Error(c, err)
		return
	}
	result, err := readRequest(c.Request.Context(), s.db, c.GetString("userId"), holder.String)
	result.QualificationStatus = state
	respond(c, result, err)
}

func (s *service) listOrders(c *gin.Context, admin bool) {
	page, size, err := httpx.Page(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	where := " WHERE o.user_id=?"
	args := []any{c.GetString("userId")}
	if admin {
		where = " WHERE 1=1"
		args = nil
	}
	if id := c.Query("activityId"); id != "" {
		if err = httpx.ID(id); err != nil {
			httpx.Error(c, err)
			return
		}
		where += " AND o.activity_id=?"
		args = append(args, id)
	}
	var total int
	err = s.db.QueryRowContext(c.Request.Context(), "SELECT COUNT(*) FROM orders o"+where, args...).Scan(&total)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	args = append(args, size, (page-1)*size)
	rows, err := s.db.QueryContext(c.Request.Context(), "SELECT "+orderColumns+orderJoin+where+" ORDER BY o.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	defer rows.Close()
	items := make([]order, 0, size)
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		items = append(items, o)
	}
	if err = rows.Err(); err != nil {
		httpx.Error(c, err)
		return
	}
	respond(c, gin.H{"items": items, "total": total, "page": page, "pageSize": size}, nil)
}

func (s *service) stats(c *gin.Context) {
	a, err := getActivity(c.Request.Context(), s.db, c.Param("id"), false)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var accepted, processing, created, failed int
	err = s.db.QueryRowContext(c.Request.Context(), `SELECT COALESCE(SUM(status='ACCEPTED'),0),COALESCE(SUM(status='PROCESSING'),0),
		COALESCE(SUM(status='ORDER_CREATED'),0),COALESCE(SUM(status='FAILED'),0) FROM activity_request WHERE activity_id=?`, a.ID).Scan(&accepted, &processing, &created, &failed)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var pending, paid, closing, canceled int
	err = s.db.QueryRowContext(c.Request.Context(), `SELECT COALESCE(SUM(status='PENDING_PAYMENT'),0),COALESCE(SUM(status='PAID'),0),
		COALESCE(SUM(status='CLOSING'),0),COALESCE(SUM(status='CANCELED'),0) FROM orders WHERE activity_id=?`, a.ID).Scan(&pending, &paid, &closing, &canceled)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	respond(c, gin.H{"activityId": a.ID, "asOf": time.Now().UTC(), "quota": a.Quota, "usedQuota": a.UsedQuota,
		"requests": gin.H{"accepted": accepted, "processing": processing, "orderCreated": created, "failed": failed},
		"orders":   gin.H{"pendingPayment": pending, "paid": paid, "closing": closing, "canceled": canceled}}, nil)
}
