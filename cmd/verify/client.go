package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type object = map[string]any
type response struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    object `json:"data"`
	Trace   string `json:"traceId"`
}
type suite struct {
	cfg                      options
	client                   *http.Client
	operator, buyer, buyerID string
	started                  time.Time
	log                      *os.File
	mu                       sync.Mutex
	logErr                   error
}

func newSuite(cfg options, dir string) (*suite, error) {
	f, err := os.Create(filepath.Join(dir, "http.jsonl"))
	if err != nil {
		return nil, err
	}
	return &suite{cfg: cfg, started: time.Now().UTC(), client: &http.Client{Timeout: 8 * time.Second}, log: f}, nil
}
func (s *suite) close() { _ = s.log.Close() }

// call 保存实际状态、耗时、traceId 和响应；登录响应含 JWT，不写入报告。
func (s *suite) call(base, path, method, token string, input any) (response, int, error) {
	var out response
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return out, 0, err
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return out, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now().UTC()
	res, err := s.client.Do(req)
	statusCode := 0
	if err == nil {
		statusCode = res.StatusCode
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if err == nil {
			err = json.Unmarshal(raw, &out)
		}
		if err == nil && out.Trace == "" {
			err = fmt.Errorf("响应缺少 traceId")
		}
	}
	if path != "/api/auth/login" {
		e := object{"startedAt": start, "durationMs": time.Since(start).Milliseconds(), "baseURL": base, "path": path, "method": method, "input": input, "httpStatus": statusCode, "response": out}
		if err != nil {
			e["error"] = err.Error()
		}
		s.mu.Lock()
		if e2 := json.NewEncoder(s.log).Encode(e); e2 != nil {
			s.logErr = e2
		}
		s.mu.Unlock()
	}
	return out, statusCode, err
}
func (s *suite) request(base, path, method, token string, input any, expected int) (object, error) {
	r, code, err := s.call(base, path, method, token, input)
	if err != nil {
		return nil, err
	}
	if code != expected {
		return nil, fmt.Errorf("%s %s: HTTP %d，期望 %d: %s %s", method, path, code, expected, r.Code, r.Message)
	}
	if expected < 300 && r.Code != "OK" {
		return nil, fmt.Errorf("%s: code=%s", path, r.Code)
	}
	return r.Data, nil
}
func (s *suite) login(name string) (string, string, error) {
	d, err := s.request(s.cfg.user, "/api/auth/login", "POST", "", object{"username": name, "password": s.cfg.password}, 200)
	if err != nil {
		return "", "", err
	}
	token := str(d, "accessToken")
	if token == "" {
		return "", "", fmt.Errorf("登录没有返回令牌")
	}
	me, err := s.request(s.cfg.user, "/api/auth/me", "GET", token, nil, 200)
	return token, str(me, "id"), err
}
func str(d object, key string) string { v, _ := d[key].(string); return v }
func num(d object, key string) int    { v, _ := d[key].(float64); return int(v) }
func obj(d object, key string) object {
	v, _ := d[key].(map[string]any)
	if v == nil {
		return object{}
	}
	return v
}
func items(d object) []any { v, _ := d["items"].([]any); return v }
func newID() string        { return uuid.NewString() }

func (s *suite) prepare(e object) error {
	var err error
	s.operator, _, err = s.login("operator")
	if err != nil {
		return err
	}
	s.buyer, s.buyerID, err = s.login("user01")
	if err != nil {
		return err
	}
	if s.buyerID == "" {
		return fmt.Errorf("/api/auth/me 未返回 id")
	}
	a, err := s.request(s.cfg.trade, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
	if err != nil {
		return err
	}
	b, err := s.request(s.cfg.peer, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
	if err != nil {
		return err
	}
	e["trade"] = a
	e["peer"] = b
	if s.cfg.mode == "faults" || s.cfg.mode == "windows" || s.cfg.mode == "all" {
		if num(a, "paymentWindowMs") > 15000 || num(b, "paymentWindowMs") > 15000 {
			return fmt.Errorf("故障验收要求两个交易实例 PAYMENT_WINDOW<=15s，使用 compose.acceptance.yaml")
		}
	}
	if str(a, "instance") == "" || str(a, "instance") == str(b, "instance") {
		return fmt.Errorf("需要两个具有不同 HOSTNAME 的交易进程，并定向访问不同实例")
	}
	for _, base := range []string{s.cfg.trade, s.cfg.peer, s.cfg.catalog, s.cfg.payment} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		if d["demoFaultsEnabled"] != true {
			return fmt.Errorf("%s 必须显式开启 DEMO_FAULTS_ENABLED 以创建独立验收数据", base)
		}
	}
	return nil
}

func (s *suite) diagnostics(e object) {
	d := object{}
	for _, entry := range []struct{ name, base string }{{"trade", s.cfg.trade}, {"peer", s.cfg.peer}, {"catalog", s.cfg.catalog}, {"payment", s.cfg.payment}} {
		r, code, err := s.call(entry.base, "/api/admin/diagnostics", "GET", s.operator, nil)
		if err == nil && code == 200 {
			d[entry.name] = r.Data
		}
	}
	for _, table := range []string{"outbox", "event_inbox", "operation_task"} {
		for _, state := range []string{"PENDING", "PROCESSING", "NEEDS_REVIEW"} {
			r, code, err := s.call(s.cfg.trade, "/api/admin/reliability/"+table+"?pageSize=100&status="+state, "GET", s.operator, nil)
			if err == nil && code == 200 {
				d[table+"."+state] = r.Data
			}
		}
	}
	e["diagnostics"] = d
}

type fixture struct{ ActivityID, SKUID string }

func (s *suite) fixture(stock int) (fixture, error) {
	sku, err := s.request(s.cfg.catalog, "/api/admin/demo/skus", "POST", s.operator, object{"name": "M2-" + newID(), "stock": stock}, 201)
	if err != nil {
		return fixture{}, err
	}
	in := object{"title": "M2-" + newID(), "skuId": str(sku, "skuId"), "priceCent": 1990, "quota": stock, "startsAt": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "endsAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
	a, err := s.request(s.cfg.trade, "/api/admin/activities", "POST", s.operator, in, 201)
	if err != nil {
		return fixture{}, err
	}
	f := fixture{ActivityID: str(a, "id"), SKUID: str(sku, "skuId")}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, code, err := s.call(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/publish", "POST", s.operator, nil)
		if err == nil && code == 200 && str(r.Data, "status") == "PUBLISHED" {
			return f, nil
		}
		if err == nil && code != 503 {
			return f, fmt.Errorf("发布失败: %d %s", code, r.Message)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return f, fmt.Errorf("发布未在 30 秒内完成")
}

func (s *suite) submit(f fixture, token, id string) (object, error) {
	return s.request(s.cfg.trade, "/api/activities/"+f.ActivityID+"/requests", "POST", token, object{"requestId": id}, 202)
}
func (s *suite) order(no, token string) (object, error) {
	return s.request(s.cfg.trade, "/api/orders/"+no, "GET", token, nil, 200)
}
func (s *suite) waitOrder(no, token, state, inventory string) (object, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		o, err := s.order(no, token)
		if err == nil && str(o, "status") == state && (inventory == "" || str(o, "inventoryStatus") == inventory) {
			return o, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("订单 %s 未在恢复健康后 30 秒内达到 %s/%s", no, state, inventory)
}
func (s *suite) created(f fixture) (object, error) {
	r, err := s.submit(f, s.buyer, newID())
	if err != nil {
		return nil, err
	}
	return s.waitOrder(str(r, "orderNo"), s.buyer, "PENDING_PAYMENT", "LOCKED")
}
func (s *suite) payment(o object) (object, error) {
	return s.request(s.cfg.trade, "/api/orders/"+str(o, "orderNo")+"/payment", "POST", s.buyer, nil, 200)
}
func (s *suite) pay(p object) error {
	_, err := s.request(s.cfg.payment, "/api/payments/"+str(p, "paymentNo")+"/simulate-success", "POST", s.buyer, nil, 200)
	return err
}
func (s *suite) waitExpiry(o object) error {
	expires, err := time.Parse(time.RFC3339Nano, str(o, "expiresAt"))
	if err != nil {
		return err
	}
	remaining := time.Until(expires)
	if remaining > 15*time.Second {
		return fmt.Errorf("故障验收要求 PAYMENT_WINDOW<=15s；使用 compose.acceptance.yaml")
	}
	if remaining > 0 {
		time.Sleep(remaining + 100*time.Millisecond)
	}
	return nil
}

// invariant 同时比较所属服务对外事实。跨服务读取非原子，须先等待场景收敛。
func (s *suite) invariant(f fixture, created, paid, canceled, available, sold int, e object) error {
	stats, err := s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/stats", "GET", s.operator, nil, 200)
	if err != nil {
		return err
	}
	stock, err := s.request(s.cfg.catalog, "/api/admin/skus/"+f.SKUID+"/inventory", "GET", s.operator, nil, 200)
	if err != nil {
		return err
	}
	admission, err := s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/admission", "GET", s.operator, nil, 200)
	if err != nil {
		return err
	}
	e["stats"] = stats
	e["stock"] = stock
	e["admission"] = admission
	reqs := obj(stats, "requests")
	orders := obj(stats, "orders")
	if num(reqs, "accepted")+num(reqs, "processing") != 0 || num(reqs, "orderCreated") != created || num(stats, "usedQuota") != created {
		return fmt.Errorf("受理或额度事实未收敛")
	}
	if num(orders, "paid") != paid || num(orders, "canceled") != canceled || num(orders, "pendingPayment") != created-paid-canceled || num(orders, "closing") != 0 {
		return fmt.Errorf("订单终态统计不符")
	}
	if num(stock, "available") != available || num(stock, "sold") != sold || num(stock, "locked") != created-paid-canceled || num(stock, "available")+num(stock, "locked")+num(stock, "sold") != num(stock, "total") {
		return fmt.Errorf("库存数量或守恒不符")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		admission, err = s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/admission", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		if str(obj(admission, "redis"), "remaining") == fmt.Sprint(num(stats, "quota")-created) && num(admission, "pendingEntries") == 1 {
			e["admission"] = admission
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("Redis 剩余额度/预扣索引未收敛")
}
