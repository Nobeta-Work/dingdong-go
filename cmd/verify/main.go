// Command verify 通过真实 HTTP 接口执行 M2 验收并落盘报告，不启动容器、不修改数据库。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type options struct {
	mode, trade, peer, user, catalog, payment, password, output string
	allowCrash                                                  bool
}

type outcome struct {
	Name       string         `json:"name"`
	StartedAt  time.Time      `json:"startedAt"`
	DurationMS int64          `json:"durationMs"`
	Passed     bool           `json:"passed"`
	Error      string         `json:"error,omitempty"`
	Evidence   map[string]any `json:"evidence"`
}

func main() {
	var cfg options
	flag.StringVar(&cfg.mode, "mode", "cache", "cache | concurrency | rate | faults | windows | all（all 不含独立 rate 配置）")
	flag.StringVar(&cfg.trade, "trade-url", "http://localhost:18082", "交易实例一")
	flag.StringVar(&cfg.peer, "trade-peer-url", "http://localhost:18084", "交易实例二，必须定向不同进程")
	flag.StringVar(&cfg.user, "user-url", "http://localhost:18080", "用户服务")
	flag.StringVar(&cfg.catalog, "catalog-url", "http://localhost:18081", "商品服务")
	flag.StringVar(&cfg.payment, "payment-url", "http://localhost:18083", "支付服务")
	flag.StringVar(&cfg.password, "password", os.Getenv("DEMO_PASSWORD"), "种子密码，推荐通过 DEMO_PASSWORD 提供")
	flag.StringVar(&cfg.output, "report-dir", ".cache/reports", "报告目录，每次执行创建独立子目录")
	flag.BoolVar(&cfg.allowCrash, "allow-crash", false, "允许演示服务主动退出；faults/windows/all 必须显式指定")
	flag.Parse()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cfg options) error {
	switch cfg.mode {
	case "cache", "concurrency", "rate", "faults", "windows", "all":
	default:
		return fmt.Errorf("未知 mode: %s", cfg.mode)
	}
	if cfg.password == "" {
		return fmt.Errorf("缺少 DEMO_PASSWORD")
	}
	if (cfg.mode == "faults" || cfg.mode == "windows" || cfg.mode == "all") && !cfg.allowCrash {
		return fmt.Errorf("该模式会主动终止演示进程，须显式指定 -allow-crash 并保证外部重启策略可用")
	}
	dir := filepath.Join(cfg.output, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	s, err := newSuite(cfg, dir)
	if err != nil {
		return err
	}
	defer s.close()
	var results []outcome
	execute := func(name string, fn func(map[string]any) error) {
		result := outcome{Name: name, StartedAt: time.Now().UTC(), Evidence: map[string]any{}}
		err := fn(result.Evidence)
		s.mu.Lock()
		if err == nil && s.logErr != nil {
			err = fmt.Errorf("保存 HTTP 证据失败: %w", s.logErr)
		}
		s.mu.Unlock()
		result.DurationMS = time.Since(result.StartedAt).Milliseconds()
		result.Passed = err == nil
		if err != nil {
			result.Error = err.Error()
			s.diagnostics(result.Evidence)
		}
		results = append(results, result)
		fmt.Printf("%s passed=%v duration=%dms %s\n", name, result.Passed, result.DurationMS, result.Error)
	}
	execute("preconditions", s.prepare)
	if results[0].Passed {
		if cfg.mode == "cache" || cfg.mode == "all" {
			execute("cache-50+50", s.cacheCase)
		}
		if cfg.mode == "concurrency" || cfg.mode == "all" {
			execute("concurrency-500-stock-200", s.concurrencyCase)
		}
		if cfg.mode == "rate" {
			execute("separate-user-and-activity-buckets", s.rateCase)
		}
		if cfg.mode == "faults" || cfg.mode == "all" {
			for group := 1; group <= 5; group++ {
				for kind := 1; kind <= 8; kind++ {
					name := fmt.Sprintf("fault-%02d-group-%d", kind, group)
					execute(name, func(e map[string]any) error { return s.faultCase(kind, e) })
				}
			}
		}
		if cfg.mode == "windows" || cfg.mode == "all" {
			for kind := 1; kind <= 4; kind++ {
				execute(fmt.Sprintf("window-%d", kind), func(e map[string]any) error { return s.windowCase(kind, e) })
			}
		}
	}
	report := map[string]any{"startedAt": s.started, "finishedAt": time.Now().UTC(), "mode": cfg.mode, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "logicalCPUs": runtime.NumCPU(), "tradeURL": cfg.trade, "tradePeerURL": cfg.peer, "results": results, "httpEvidence": "http.jsonl", "scope": "客户端观察；数据库最终一致性另在静止窗口运行 diagnose"}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), body, 0644); err != nil {
		return err
	}
	fmt.Printf("实际运行报告：%s\n", filepath.Join(dir, "report.json"))
	for _, r := range results {
		if !r.Passed {
			return fmt.Errorf("验收存在失败，详见报告和 http.jsonl；未将超时视为成功")
		}
	}
	return nil
}
