// Package app 负责基础设施的装配与生命周期；业务实现仍留在各服务自己的包中。
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/balancer/roundrobin" // 注册 DNS 多地址的轮询策略。
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/config"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/httpx"
)

// Spec 只描述各服务实际使用的依赖，不让公共启动层理解业务数据。
type Spec struct {
	Name         string
	Redis        bool
	MQ           bool
	RPC          bool
	TradeClients bool
}

// Run 装配依赖、启动监听并在退出信号到来时排空在途请求。
func Run(spec Spec) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", spec.Name, "instance", os.Getenv("HOSTNAME"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// * 驱动参数在此统一设置，所有服务按 UTC 读写 DATETIME，禁止多语句执行。
	dbCfg, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("MYSQL_DSN 格式错误: %w", err)
	}
	dbCfg.ParseTime, dbCfg.Loc, dbCfg.MultiStatements = true, time.UTC, false
	if dbCfg.Params == nil {
		dbCfg.Params = make(map[string]string)
	}
	dbCfg.Params["time_zone"] = "'+00:00'"
	dbCfg.Timeout, dbCfg.ReadTimeout, dbCfg.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	db, err := sql.Open("mysql", dbCfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("打开 MySQL: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	checks := map[string]func(context.Context) error{"mysql": db.PingContext}
	if spec.Redis {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB,
			DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
		defer rdb.Close()
		checks["redis"] = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
	}
	if spec.MQ {
		// 仅探测网络连通性；真实生产、消费及重投由 mqcheck 验证，不冒充业务消费者就绪。
		checks["mq_nameserver_tcp"] = tcpCheck(cfg.Nameserver)
		checks["mq_broker_tcp"] = tcpCheck(cfg.BrokerAddr)
	}
	if spec.TradeClients {
		for name, target := range map[string]string{"catalog_rpc": cfg.CatalogTarget, "payment_rpc": cfg.PaymentTarget} {
			// ClientConn 长期复用。写操作未来显式指定业务重试，不安装全局重试拦截器。
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`))
			if err != nil {
				return fmt.Errorf("创建 %s 连接: %w", name, err)
			}
			defer conn.Close()
			client := healthpb.NewHealthClient(conn)
			checks[name] = func(ctx context.Context) error {
				res, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
				if err != nil {
					return err
				}
				if res.Status != healthpb.HealthCheckResponse_SERVING {
					return fmt.Errorf("RPC 未就绪")
				}
				return nil
			}
		}
	}

	// 依赖未就绪时启动失败并给出明确日志；Compose 会按重启策略重试。
	if failed := checkAll(ctx, checks); len(failed) != 0 {
		return fmt.Errorf("依赖未就绪: %v", failed)
	}
	var draining atomic.Bool
	router := httpx.NewRouter(logger)
	router.GET("/livez", func(c *gin.Context) { httpx.Write(c, 200, "OK", "进程存活", nil) })
	router.GET("/readyz", func(c *gin.Context) {
		if draining.Load() {
			httpx.Write(c, 503, "NOT_READY", "服务正在退出", nil)
			return
		}
		failed := checkAll(c.Request.Context(), checks)
		if len(failed) != 0 {
			logger.Warn("dependencies_unavailable", "traceId", c.GetString("traceId"), "dependencies", failed)
			httpx.Write(c, 503, "DEPENDENCY_UNAVAILABLE", "依赖尚未就绪", failed)
			return
		}
		httpx.Write(c, 200, "OK", "基础设施就绪", gin.H{"service": spec.Name, "stage": "M1_FRAMEWORK"})
	})
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("监听 HTTP: %w", err)
	}
	defer httpListener.Close()
	serverErrors := make(chan error, 2)
	var rpcServer *grpc.Server
	var healthServer *health.Server
	if spec.RPC {
		listener, err := net.Listen("tcp", cfg.RPCAddr)
		if err != nil {
			return fmt.Errorf("监听 RPC: %w", err)
		}
		defer listener.Close()
		rpcServer = grpc.NewServer()
		healthServer = health.NewServer()
		healthpb.RegisterHealthServer(rpcServer, healthServer)
		// 框架阶段只注册标准健康协议；业务 RPC 尚未注册，调用会返回 Unimplemented。
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
		go func() { serverErrors <- rpcServer.Serve(listener) }()
	}
	go func() { serverErrors <- httpServer.Serve(httpListener) }()
	logger.Info("service_started", "httpAddr", cfg.HTTPAddr, "rpcEnabled", spec.RPC, "stage", "M1_FRAMEWORK")
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverErrors:
	}

	// # 退出阶段：先撤销就绪，再排空监听，最后由 defer 关闭依赖连接。
	draining.Store(true)
	if healthServer != nil {
		healthServer.Shutdown()
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	rpcDone := make(chan struct{})
	go func() {
		if rpcServer != nil {
			rpcServer.GracefulStop()
		}
		close(rpcDone)
	}()
	if err := httpServer.Shutdown(drainCtx); err != nil {
		logger.Warn("http_drain_timeout", "error", err)
		_ = httpServer.Close()
	}
	select {
	case <-rpcDone:
	case <-drainCtx.Done():
		if rpcServer != nil {
			rpcServer.Stop()
			<-rpcDone
		}
	}
	logger.Info("service_stopped")
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return serveErr
	}
	return nil
}

func checkAll(parent context.Context, checks map[string]func(context.Context) error) []string {
	// 每项独立 1 秒上限且并行执行，依赖故障时探针整体不会线性累加等待。
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(checks))
	for name, check := range checks {
		go func() {
			ctx, cancel := context.WithTimeout(parent, time.Second)
			defer cancel()
			results <- result{name, check(ctx)}
		}()
	}
	failed := make([]string, 0)
	for range checks {
		if r := <-results; r.err != nil {
			failed = append(failed, r.name)
		}
	}
	return failed
}

func tcpCheck(addr string) func(context.Context) error {
	return func(ctx context.Context) error {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}
