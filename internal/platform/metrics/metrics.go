// Package metrics 提供有界指标名的进程计数器；业务 ID 不作为指标标签，避免无限增长。
package metrics

import (
	"sync"
	"time"
)

// Registry 保存单个服务实例的计数及耗时汇总，通过运营诊断接口读取。
type Registry struct {
	mu     sync.Mutex
	values map[string]int64
}

// New 创建空的指标集合。
func New() *Registry { return &Registry{values: make(map[string]int64)} }

// Add 累加固定名称的计数。
func (r *Registry) Add(name string, n int64) { r.mu.Lock(); r.values[name] += n; r.mu.Unlock() }

// Observe 记录次数、总耗时与最大耗时，单位为微秒，不把平均值伪装为分位数。
func (r *Registry) Observe(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[name+".count"]++
	r.values[name+".sum_us"] += d.Microseconds()
	if d.Microseconds() > r.values[name+".max_us"] {
		r.values[name+".max_us"] = d.Microseconds()
	}
}

// Snapshot 返回副本，不向调用方暴露内部可变 map。
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := make(map[string]int64, len(r.values))
	for key, value := range r.values {
		copy[key] = value
	}
	return copy
}
