// Package config 读取进程环境变量；容器和 IDE 使用同一套配置入口。
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Config 保存单个服务的基础设施配置，不承载其他服务的数据库凭据。
type Config struct {
	HTTPAddr        string
	RPCAddr         string
	MySQLDSN        string
	RedisAddr       string
	RedisPassword   string
	RedisDB         int
	Nameserver      string
	BrokerAddr      string
	CatalogTarget   string
	PaymentTarget   string
	ShutdownTimeout time.Duration
	JWTSecret       string
	PaymentWindow   time.Duration
	UserBurst       int
	UserRate        int
	ActivityBurst   int
	ActivityRate    int
	CacheOrigins    int
	Workers         int
	RetryLimit      int
	DemoFaults      bool
}

// Load 在监听端口前校验必需配置，避免服务带着无效配置对外就绪。
func Load() (Config, error) {
	c := Config{
		HTTPAddr:        value("HTTP_ADDR", ":8080"),
		RPCAddr:         value("RPC_ADDR", ":9000"),
		MySQLDSN:        os.Getenv("MYSQL_DSN"),
		RedisAddr:       value("REDIS_ADDR", "localhost:16379"),
		RedisPassword:   os.Getenv("REDIS_PASSWORD"),
		Nameserver:      value("ROCKETMQ_NAMESERVER", "localhost:19876"),
		BrokerAddr:      value("ROCKETMQ_BROKER_ADDR", "localhost:10911"),
		CatalogTarget:   value("CATALOG_RPC_TARGET", "dns:///localhost:19000"),
		PaymentTarget:   value("PAYMENT_RPC_TARGET", "dns:///localhost:19001"),
		ShutdownTimeout: 15 * time.Second,
		JWTSecret:       os.Getenv("JWT_SECRET"),
	}
	if c.MySQLDSN == "" {
		return Config{}, fmt.Errorf("缺少 MYSQL_DSN，请使用当前服务独立的数据库账号")
	}
	if len(c.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("JWT_SECRET 至少需要 32 字节")
	}
	window, err := time.ParseDuration(value("PAYMENT_WINDOW", "120s"))
	if err != nil || window < 5*time.Second || window > time.Hour {
		return Config{}, fmt.Errorf("PAYMENT_WINDOW 必须为 5s~1h 的时长")
	}
	c.PaymentWindow = window
	for _, entry := range []struct {
		key                string
		fallback, min, max int
		target             *int
	}{
		{"USER_RATE_BURST", 3, 1, 10000, &c.UserBurst}, {"USER_RATE_PER_SECOND", 1, 1, 10000, &c.UserRate},
		{"ACTIVITY_RATE_BURST", 500, 1, 100000, &c.ActivityBurst}, {"ACTIVITY_RATE_PER_SECOND", 100, 1, 100000, &c.ActivityRate},
		{"CACHE_ORIGIN_LIMIT", 16, 1, 100, &c.CacheOrigins}, {"WORKER_CONCURRENCY", 8, 1, 8, &c.Workers}, {"RETRY_LIMIT", 20, 1, 100, &c.RetryLimit},
	} {
		n, parseErr := strconv.Atoi(value(entry.key, strconv.Itoa(entry.fallback)))
		if parseErr != nil || n < entry.min || n > entry.max {
			return Config{}, fmt.Errorf("%s 必须在 %d~%d 之间", entry.key, entry.min, entry.max)
		}
		*entry.target = n
	}
	c.DemoFaults, err = strconv.ParseBool(value("DEMO_FAULTS_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("DEMO_FAULTS_ENABLED 必须为 true 或 false")
	}
	for _, addr := range []string{c.HTTPAddr, c.RPCAddr, c.RedisAddr, c.Nameserver, c.BrokerAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return Config{}, fmt.Errorf("地址 %q 应为 host:port: %w", addr, err)
		}
	}
	c.RedisDB, err = strconv.Atoi(value("REDIS_DB", "0"))
	if err != nil || c.RedisDB < 0 {
		return Config{}, fmt.Errorf("REDIS_DB 必须是非负整数")
	}
	return c, nil
}

func value(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
