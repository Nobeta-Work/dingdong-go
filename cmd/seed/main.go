// Command seed 按服务独立连接所属库，幂等初始化账号或 SKU，不重置已有库存。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	service := flag.String("service", "", "目标服务：user 或 catalog")
	flag.Parse()
	if err := run(*service); err != nil {
		slog.Error("seed_failed", "error", err)
		os.Exit(1)
	}
}

func run(service string) error {
	if service != "user" && service != "catalog" {
		return fmt.Errorf("-service 必须为 user 或 catalog")
	}
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		return fmt.Errorf("缺少当前服务的 MYSQL_DSN")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if service == "user" {
		password := os.Getenv("DEMO_PASSWORD")
		if len(password) < 8 || len(password) > 72 {
			return fmt.Errorf("DEMO_PASSWORD 长度必须为 8~72 字节")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		accounts := []string{"user01", "user02", "operator"}
		for i := 1; i <= 500; i++ {
			accounts = append(accounts, fmt.Sprintf("bench%04d", i))
		}
		for _, username := range accounts {
			role := "USER"
			if username == "operator" {
				role = "OPERATOR"
			}
			// 重复初始化保持已有哈希和角色，不覆盖已修改的账号。
			_, err = tx.ExecContext(ctx, "INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE username = users.username", username, string(hash), role)
			if err != nil {
				return err
			}
		}
	} else {
		for id := 1; id <= 2; id++ {
			_, err = tx.ExecContext(ctx, "INSERT INTO sku (id, name, price_cent) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE id = sku.id", id, fmt.Sprintf("叮咚演示商品%d", id), 1990)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO inventory (sku_id, total, available) VALUES (?, 200, 200) ON DUPLICATE KEY UPDATE sku_id = inventory.sku_id", id)
			if err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("seed_completed", "service", service)
	return nil
}
