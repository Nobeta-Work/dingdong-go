// Package user 用户服务拥有账号和角色，提供密码校验、JWT 签发和当前用户查询。
package user

import "github.com/Nobeta-Work/dingdong-go/internal/platform/app"

// Run 启动用户服务的 HTTP 和数据库依赖，账号业务在当前包内实现。
func Run() error {
	return app.Run(app.Spec{Name: "user-service", Configure: configure})
}
