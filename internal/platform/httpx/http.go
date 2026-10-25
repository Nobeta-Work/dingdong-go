// Package httpx 提供 HTTP 协议约定和中间件，不包含商城业务规则。
package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Response 是 PRD 约定的统一响应；业务 ID 由业务 DTO 明确声明为 string。
type Response struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	TraceID string `json:"traceId"`
}

// Write 使用同一个请求追踪号关联接口响应和结构化日志。
func Write(c *gin.Context, status int, code, message string, data any) {
	c.JSON(status, Response{Code: code, Message: message, Data: data, TraceID: c.GetString("traceId")})
}

// NewRouter 安装基础中间件，未知路径明确返回 404，不伪造未实现的业务接口。
func NewRouter(logger *slog.Logger) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// 由服务端生成，避免把任意客户端身份头或超长文本写入日志。
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		traceID := hex.EncodeToString(id)
		c.Set("traceId", traceID)
		c.Header("X-Trace-ID", traceID)
		start := time.Now()
		c.Next()
		logger.Info("http_request", "traceId", traceID, "method", c.Request.Method,
			"path", c.Request.URL.Path, "status", c.Writer.Status(), "durationMs", time.Since(start).Milliseconds())
	})
	r.Use(func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("http_panic", "traceId", c.GetString("traceId"), "error", err)
				c.Abort()
				Write(c, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误", nil)
			}
		}()
		c.Next()
	})
	r.HandleMethodNotAllowed = true
	r.NoRoute(func(c *gin.Context) { Write(c, 404, "NOT_FOUND", "接口不存在", nil) })
	r.NoMethod(func(c *gin.Context) { Write(c, 405, "METHOD_NOT_ALLOWED", "请求方法不支持", nil) })
	return r
}
