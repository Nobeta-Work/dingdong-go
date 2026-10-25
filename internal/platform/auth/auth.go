// Package auth 在各 HTTP 服务复用 JWT 协议；用户身份只来自验签后的 Claims。
package auth

import (
	"strconv"
	"strings"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Claims 包含服务端签发的用户 ID、角色和标准有效期字段。
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Token 签发两小时有效的 HS256 令牌；所有服务共享同一个 Demo 密钥。
func Token(secret, userID, role string) (string, time.Time, error) {
	expires := time.Now().UTC().Add(2 * time.Hour)
	c := Claims{Role: role, RegisteredClaims: jwt.RegisteredClaims{Issuer: "dingdong", Subject: userID,
		IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(expires)}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	return token, expires, err
}

// Require 校验签名、算法、签发者、有效期及角色，绝不读取客户端 userId 头。
func Require(secret string, operator bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := strings.Fields(c.GetHeader("Authorization"))
		if len(header) != 2 || !strings.EqualFold(header[0], "Bearer") {
			reject(c)
			return
		}
		claims := &Claims{}
		_, err := jwt.ParseWithClaims(header[1], claims, func(*jwt.Token) (any, error) { return []byte(secret), nil },
			jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("dingdong"), jwt.WithExpirationRequired())
		id, idErr := strconv.ParseUint(claims.Subject, 10, 64)
		if err != nil || idErr != nil || id == 0 || (claims.Role != "USER" && claims.Role != "OPERATOR") {
			reject(c)
			return
		}
		if operator && claims.Role != "OPERATOR" {
			c.Abort()
			httpx.Write(c, 403, "FORBIDDEN", "需要运营权限", nil)
			return
		}
		c.Set("userId", claims.Subject)
		c.Set("role", claims.Role)
		c.Next()
	}
}

func reject(c *gin.Context) {
	c.Abort()
	httpx.Write(c, 401, "UNAUTHORIZED", "请登录或重新获取有效令牌", nil)
}
