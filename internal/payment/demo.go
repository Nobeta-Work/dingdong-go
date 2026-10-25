package payment

import (
	pb "github.com/Nobeta-Work/dingdong-go/api/gen/payment/v1"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// registerDemo 复用支付业务方法，演示创建/关闭竞争以及关闭先到的墓碑语义。
func registerDemo(r *app.Runtime, s *service) {
	if !r.Config.DemoFaults {
		return
	}
	g := r.Router.Group("/api/admin/demo/payments", auth.Require(r.Config.JWTSecret, true))
	g.POST("/:action", func(c *gin.Context) {
		var input struct {
			OrderNo    string `json:"orderNo" binding:"required,max=64"`
			UserID     string `json:"userId"`
			AmountCent int64  `json:"amountCent"`
			ExpiresAt  int64  `json:"expiresAtUnixMilli"`
		}
		if c.ShouldBindJSON(&input) != nil {
			httpx.Write(c, 400, "INVALID_ARGUMENT", "支付字段不合法", nil)
			return
		}
		var result *pb.PaymentRecord
		var err error
		ctx := c.Request.Context()
		switch c.Param("action") {
		case "create":
			result, err = s.CreateOrGet(ctx, &pb.CreatePaymentRequest{OrderNo: input.OrderNo, UserId: input.UserID, AmountCent: input.AmountCent, ExpiresAtUnixMilli: input.ExpiresAt})
		case "close":
			result, err = s.CloseByOrder(ctx, &pb.OrderRequest{OrderNo: input.OrderNo})
		case "query":
			result, err = s.QueryByOrder(ctx, &pb.OrderRequest{OrderNo: input.OrderNo})
		default:
			httpx.Write(c, 404, "NOT_FOUND", "未知动作", nil)
			return
		}
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "演示支付事务完成", view(result))
	})
}
