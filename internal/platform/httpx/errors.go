package httpx

import (
	"database/sql"
	"errors"
	"log/slog"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error 将业务层与 RPC 的明确拒绝映射为 HTTP 状态，隐藏数据库等内部错误详情。
func Error(c *gin.Context, err error) {
	var duplicate *mysql.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == 1062 {
		Write(c, 409, "CONFLICT", "业务键已存在，请使用原参数查询", nil)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		Write(c, 404, "NOT_FOUND", "对象不存在", nil)
		return
	}
	s, ok := status.FromError(err)
	if !ok {
		slog.Error("http_dependency_error", "traceId", c.GetString("traceId"), "error", err)
		Write(c, 503, "DEPENDENCY_UNAVAILABLE", "依赖暂不可用，请稍后重试", nil)
		return
	}
	httpStatus, code := 503, "DEPENDENCY_UNAVAILABLE"
	switch s.Code() {
	case codes.InvalidArgument:
		httpStatus, code = 400, "INVALID_ARGUMENT"
	case codes.NotFound:
		httpStatus, code = 404, "NOT_FOUND"
	case codes.PermissionDenied:
		httpStatus, code = 403, "FORBIDDEN"
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		httpStatus, code = 409, "CONFLICT"
	case codes.Unauthenticated:
		httpStatus, code = 401, "UNAUTHORIZED"
	case codes.ResourceExhausted:
		httpStatus, code = 429, "RATE_LIMITED"
	}
	message := s.Message()
	if httpStatus == 503 {
		message = "依赖暂不可用，请稍后重试"
	}
	Write(c, httpStatus, code, message, nil)
}

// Page 校验分页参数，限制每页最多 100 条，避免无界加载。
func Page(c *gin.Context) (int, int, error) {
	page, e1 := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, e2 := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if e1 != nil || e2 != nil || page < 1 || page > 1000000 || size < 1 || size > 100 {
		return 0, 0, status.Error(codes.InvalidArgument, "page 或 pageSize 不合法")
	}
	return page, size, nil
}

// ID 校验 URL 中的正整数 ID，业务层仍用字符串传递以避免 JSON 精度问题。
func ID(value string) error {
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		return status.Error(codes.InvalidArgument, "ID 必须是正整数")
	}
	return nil
}
