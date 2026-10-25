package user

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/app"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/auth"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type profile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func configure(r *app.Runtime) (func(context.Context) error, error) {
	r.Router.POST("/api/auth/login", func(c *gin.Context) {
		var input struct {
			Username string `json:"username" binding:"required,max=64"`
			Password string `json:"password" binding:"required,max=72"`
		}
		if c.ShouldBindJSON(&input) != nil {
			httpx.Error(c, status.Error(codes.InvalidArgument, "账号或密码格式不合法"))
			return
		}
		var u profile
		var hash string
		err := r.DB.QueryRowContext(c.Request.Context(), "SELECT id, username, role, password_hash FROM users WHERE username=?", input.Username).Scan(&u.ID, &u.Username, &u.Role, &hash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			httpx.Error(c, err)
			return
		}
		if errors.Is(err, sql.ErrNoRows) || bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Password)) != nil {
			httpx.Error(c, status.Error(codes.Unauthenticated, "账号或密码错误"))
			return
		}
		token, expires, err := auth.Token(r.Config.JWTSecret, u.ID, u.Role)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "登录成功", gin.H{"accessToken": token, "expiresAt": expires, "user": u})
	})
	r.Router.GET("/api/auth/me", auth.Require(r.Config.JWTSecret, false), func(c *gin.Context) {
		var u profile
		err := r.DB.QueryRowContext(c.Request.Context(), "SELECT id, username, role FROM users WHERE id=?", c.GetString("userId")).Scan(&u.ID, &u.Username, &u.Role)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.Write(c, 200, "OK", "查询成功", u)
	})
	return nil, nil
}
