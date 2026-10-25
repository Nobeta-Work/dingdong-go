package payment

import (
	"context"
	"time"

	pb "github.com/Nobeta-Work/dingdong-go/api/gen/payment/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func configure(r *app.Runtime) (func(context.Context) error, error) {
	s := &service{db: r.DB}
	pb.RegisterPaymentServer(r.RPC, s)
	routes := r.Router.Group("/api/payments", auth.Require(r.Config.JWTSecret, false))
	routes.GET("/:paymentNo", func(c *gin.Context) {
		var order string
		err := r.DB.QueryRowContext(c.Request.Context(), "SELECT order_no FROM payment WHERE payment_no=?", c.Param("paymentNo")).Scan(&order)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		p, err := read(c.Request.Context(), r.DB, order, false)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		if p.UserId != c.GetString("userId") {
			httpx.Error(c, status.Error(codes.PermissionDenied, "不能查询他人支付单"))
			return
		}
		httpx.Write(c, 200, "OK", "模拟支付结果", view(p))
	})
	routes.POST("/:paymentNo/simulate-success", func(c *gin.Context) {
		p, err := s.simulate(c.Request.Context(), c.Param("paymentNo"), c.GetString("userId"), c.GetString("traceId"))
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "模拟支付成功，未产生真实扣款", view(p))
	})
	bus, err := event.Start(r.DB, r.Config.Nameserver, "dingdong-payment", r.Logger, nil)
	if err != nil {
		return nil, err
	}
	return bus.Close, nil
}

func view(p *pb.PaymentRecord) gin.H {
	states := map[pb.PaymentStatus]string{pb.PaymentStatus_PAYMENT_STATUS_PENDING: "PENDING", pb.PaymentStatus_PAYMENT_STATUS_SUCCESS: "SUCCESS", pb.PaymentStatus_PAYMENT_STATUS_CLOSED: "CLOSED"}
	var expires any
	if p.ExpiresAtUnixMilli > 0 {
		expires = time.UnixMilli(p.ExpiresAtUnixMilli).UTC()
	}
	return gin.H{"paymentNo": p.PaymentNo, "orderNo": p.OrderNo, "userId": p.UserId, "amountCent": p.AmountCent, "expiresAt": expires, "status": states[p.Status], "simulated": true}
}
