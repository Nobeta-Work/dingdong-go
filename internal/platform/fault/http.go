package fault

import (
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
)

// Register 只对已登录运营暴露 Demo 注入接口；未开启时返回 404，匿名请求不能设置故障。
func (e *Engine) Register(router *gin.Engine, secret string) {
	g := router.Group("/api/admin/faults", auth.Require(secret, true), func(c *gin.Context) {
		if !e.Enabled {
			c.Abort()
			httpx.Write(c, 404, "NOT_FOUND", "Demo 故障注入未开启", nil)
			return
		}
		c.Next()
	})
	g.GET("", func(c *gin.Context) { httpx.Write(c, 200, "OK", "当前实例故障规则", gin.H{"rules": e.Rules()}) })
	g.POST("", func(c *gin.Context) {
		var r Rule
		if c.ShouldBindJSON(&r) != nil {
			httpx.Write(c, 400, "INVALID_ARGUMENT", "故障规则格式错误", nil)
			return
		}
		if err := e.Set(r); err != nil {
			httpx.Write(c, 400, "INVALID_ARGUMENT", err.Error(), nil)
			return
		}
		httpx.Write(c, 200, "OK", "规则已设置", gin.H{"rules": e.Rules()})
	})
	g.DELETE("/:point", func(c *gin.Context) {
		e.Clear(c.Param("point"), c.Query("key"))
		httpx.Write(c, 200, "OK", "规则已清理", nil)
	})
}
