// Package fault 只在显式开启的 Demo 环境中提供故障注入，不保存业务状态。
package fault

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/metrics"
)

// Rule 将故障限定到已注册的位置及可选业务键，并设置次数与自动过期时间。
type Rule struct {
	Point     string    `json:"point"`
	Key       string    `json:"key"`
	Action    string    `json:"action"`
	Remaining int       `json:"remaining"`
	DelayMS   int       `json:"delayMs"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Engine 管理当前实例的故障开关；多实例验收必须定向注入并记录实际实例。
type Engine struct {
	Enabled bool
	mu      sync.Mutex
	allowed map[string]bool
	rules   map[string]Rule
	metrics *metrics.Registry
}

// New 注册本服务允许的注入位置，禁止通过任意字符串开启未知行为。
func New(enabled bool, registry *metrics.Registry, points ...string) *Engine {
	e := &Engine{Enabled: enabled, allowed: make(map[string]bool), rules: make(map[string]Rule), metrics: registry}
	for _, point := range points {
		e.allowed[point] = true
	}
	return e
}

// Set 校验注入规则；所有规则最多保留五分钟，避免遗留开关长期干扰演示。
func (e *Engine) Set(r Rule) error {
	if !e.Enabled {
		return fmt.Errorf("Demo 故障注入未开启")
	}
	if !e.allowed[r.Point] || (r.Action != "fail" && r.Action != "delay" && r.Action != "crash") ||
		r.Remaining < 1 || r.Remaining > 1000 || r.DelayMS < 0 || r.DelayMS > 60000 || len(r.Key) > 128 {
		return fmt.Errorf("故障位置、动作、次数或时长不合法")
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = time.Now().UTC().Add(time.Minute)
	}
	if !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		return fmt.Errorf("故障过期时间须在未来五分钟内")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.rules) >= 100 {
		return fmt.Errorf("故障规则数量已达上限，请先清理")
	}
	e.rules[r.Point+":"+r.Key] = r
	return nil
}

// Clear 删除指定位置与业务键的规则。
func (e *Engine) Clear(point, key string) { e.mu.Lock(); delete(e.rules, point+":"+key); e.mu.Unlock() }

// Rules 返回当前规则的副本，包含剩余次数，便于验收确认实际注入。
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]Rule, 0, len(e.rules))
	for _, r := range e.rules {
		result = append(result, r)
	}
	return result
}

// Hit 在业务指定位置注入失败、延迟或进程退出。crash 仅限已授权的 Demo 故障验收。
func (e *Engine) Hit(ctx context.Context, point, key string) error {
	if !e.Enabled {
		return nil
	}
	e.mu.Lock()
	name := point + ":" + key
	r, ok := e.rules[name]
	if !ok {
		name = point + ":"
		r, ok = e.rules[name]
	}
	if !ok || r.Remaining == 0 || time.Now().After(r.ExpiresAt) {
		e.mu.Unlock()
		return nil
	}
	r.Remaining--
	e.rules[name] = r
	e.mu.Unlock()
	e.metrics.Add("fault.hit."+point, 1)
	if r.Action == "crash" {
		os.Exit(86)
	}
	if r.Action == "delay" {
		timer := time.NewTimer(time.Duration(r.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("Demo fault: %s", point)
}
