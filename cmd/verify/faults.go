package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func containsAction(body, key string) bool { return strings.Contains(body, `"`+key+`"`) }

type injection struct {
	base, point, key string
	remaining        int
}

func (s *suite) inject(base, point, key, action string, remaining, delay int) (injection, error) {
	i := injection{base, point, key, remaining}
	_, err := s.request(base, "/api/admin/faults", "POST", s.operator, object{"point": point, "key": key, "action": action, "remaining": remaining, "delayMs": delay}, 200)
	return i, err
}
func (s *suite) clear(i injection) error {
	_, err := s.request(i.base, "/api/admin/faults/"+i.point+"?key="+url.QueryEscape(i.key), "DELETE", s.operator, nil, 200)
	return err
}
func (s *suite) await(label string, predicate func() (bool, error)) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := predicate()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s 未在 30 秒内完成", label)
}
func (s *suite) hit(i injection) error {
	return s.await("故障点实际命中 "+i.point, func() (bool, error) {
		d, err := s.request(i.base, "/api/admin/faults", "GET", s.operator, nil, 200)
		if err != nil {
			return false, err
		}
		rules, _ := d["rules"].([]any)
		for _, v := range rules {
			r, ok := v.(map[string]any)
			if ok && str(r, "point") == i.point && str(r, "key") == i.key && num(r, "remaining") < i.remaining {
				return true, nil
			}
		}
		return false, nil
	})
}
func (s *suite) outbox(base, no, kind string) (object, error) {
	d, err := s.request(base, "/api/admin/reliability/outbox?key="+url.QueryEscape(no), "GET", s.operator, nil, 200)
	if err != nil {
		return nil, err
	}
	for _, v := range items(d) {
		r, ok := v.(map[string]any)
		if !ok {
			continue
		}
		var payload object
		if json.Unmarshal([]byte(str(r, "payload")), &payload) == nil && str(payload, "eventType") == kind {
			return r, nil
		}
	}
	return nil, fmt.Errorf("缺少 %s 的 %s Outbox", no, kind)
}
func (s *suite) waitSent(base, no, kind string) (object, error) {
	var row object
	err := s.await("Outbox 投递 "+kind, func() (bool, error) {
		var err error
		row, err = s.outbox(base, no, kind)
		return str(row, "status") == "SENT", err
	})
	return row, err
}
func (s *suite) replay(base string, row object) error {
	_, err := s.request(base, "/api/admin/reliability/outbox/"+str(row, "id")+"/replay", "POST", s.operator, nil, 200)
	return err
}

func (s *suite) duplicateDelivered(row object) error {
	var payload object
	if err := json.Unmarshal([]byte(str(row, "payload")), &payload); err != nil {
		return err
	}
	id := str(payload, "eventId")
	return s.await("同一事件真实重复到达 Inbox", func() (bool, error) {
		d, err := s.request(s.cfg.trade, "/api/admin/reliability/event_inbox?key="+url.QueryEscape(id), "GET", s.operator, nil, 200)
		if err != nil {
			return false, err
		}
		for _, v := range items(d) {
			r, ok := v.(map[string]any)
			if ok && num(r, "deliveries") >= 2 && str(r, "status") == "DONE" {
				return true, nil
			}
		}
		return false, nil
	})
}

// faultCase 的八个场景各执行五次；每次用独立 SKU/活动，不重置已有业务数据。
func (s *suite) faultCase(kind int, e object) (resultErr error) {
	f, err := s.fixture(1)
	if err != nil {
		return err
	}
	e["fixture"] = f
	e["kind"] = kind
	var rules []injection
	var healthyAt time.Time
	defer func() {
		if !healthyAt.IsZero() {
			elapsed := time.Since(healthyAt)
			e["recoveryDurationMs"] = elapsed.Milliseconds()
			if resultErr == nil && elapsed > 30*time.Second {
				resultErr = fmt.Errorf("恢复健康后整体收敛超过 30 秒")
			}
		}
		for _, i := range rules {
			if err := s.clear(i); err != nil && resultErr == nil {
				resultErr = fmt.Errorf("故障清理失败: %w", err)
			}
		}
	}()
	set := func(base, point, key, action string, n, delay int) (injection, error) {
		i, err := s.inject(base, point, key, action, n, delay)
		if err == nil {
			rules = append(rules, i)
		}
		return i, err
	}
	if kind == 7 {
		before, err := s.boots()
		if err != nil {
			return err
		}
		for _, base := range []string{s.cfg.trade, s.cfg.peer} {
			if _, err = set(base, "trade.after_order_commit", "", "crash", 1, 0); err != nil {
				return err
			}
		}
		reqID := newID()
		// HTTP 响应也可能因进程退出丢失；结果必须按原 requestId 回查，不能换 ID 重提。
		_, _, _ = s.call(s.cfg.trade, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": reqID})
		if err = s.awaitRestart(before); err != nil {
			return err
		}
		healthyAt = time.Now()
		r, err := s.request(s.cfg.trade, "/api/requests/"+reqID, "GET", s.buyer, nil, 200)
		if err != nil {
			return err
		}
		no := str(r, "orderNo")
		e["requestId"] = reqID
		e["orderNo"] = no
		o, err := s.order(no, s.buyer)
		if err != nil {
			return err
		}
		expires, err := time.Parse(time.RFC3339Nano, str(o, "expiresAt"))
		if err != nil {
			return err
		}
		if str(o, "status") != "PENDING_PAYMENT" || time.Until(expires) < 2*time.Second {
			if _, err = s.waitOrder(no, s.buyer, "CANCELED", "RELEASED"); err != nil {
				return err
			}
			return s.invariant(f, 1, 0, 1, 1, 0, e)
		}
		p, err := s.payment(o)
		if err != nil {
			return err
		}
		if err = s.pay(p); err != nil {
			return err
		}
		if _, err = s.waitOrder(no, s.buyer, "PAID", "CONFIRMED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 1, 0, 0, 1, e)
	}
	o, err := s.created(f)
	if err != nil {
		return err
	}
	no := str(o, "orderNo")
	e["orderNo"] = no
	p, err := s.payment(o)
	if err != nil {
		return err
	}
	switch kind {
	case 1: // 原消息发送后再真实重投，Inbox 按 eventId 去重。
		healthyAt = time.Now()
		if err = s.pay(p); err != nil {
			return err
		}
		row, err := s.waitSent(s.cfg.payment, no, "PaymentSucceeded")
		if err != nil {
			return err
		}
		if err = s.replay(s.cfg.payment, row); err != nil {
			return err
		}
		if _, err = s.waitSent(s.cfg.payment, no, "PaymentSucceeded"); err != nil {
			return err
		}
		if err = s.duplicateDelivered(row); err != nil {
			return err
		}
		if _, err = s.waitOrder(no, s.buyer, "PAID", "CONFIRMED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 1, 0, 0, 1, e)
	case 2: // 支付事实已成功，通知被阻断；到期扫描必须先查询支付事实。
		i, err := set(s.cfg.payment, "payment.before_outbox_send", no, "fail", 1000, 0)
		if err != nil {
			return err
		}
		if err = s.pay(p); err != nil {
			return err
		}
		if err = s.hit(i); err != nil {
			return err
		}
		if err = s.waitExpiry(o); err != nil {
			return err
		}
		if _, err = s.waitOrder(no, s.buyer, "PAID", "CONFIRMED"); err != nil {
			return err
		}
		if err = s.clear(i); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitSent(s.cfg.payment, no, "PaymentSucceeded"); err != nil {
			return err
		}
		return s.invariant(f, 1, 1, 0, 0, 1, e)
	case 3:
		if err = s.waitExpiry(o); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitOrder(no, s.buyer, "CANCELED", "RELEASED"); err != nil {
			return err
		}
		if _, err = s.request(s.cfg.payment, "/api/payments/"+str(p, "paymentNo")+"/simulate-success", "POST", s.buyer, nil, 409); err != nil {
			return err
		}
		fact, err := s.request(s.cfg.payment, "/api/payments/"+str(p, "paymentNo"), "GET", s.buyer, nil, 200)
		if err != nil {
			return err
		}
		if str(fact, "status") != "CLOSED" {
			return fmt.Errorf("迟到支付改变 CLOSED 终态")
		}
		e["paymentFact"] = fact
		return s.invariant(f, 1, 0, 1, 1, 0, e)
	case 4:
		i, err := set(s.cfg.payment, "payment.after_close_commit", no, "delay", 1, 2000)
		if err != nil {
			return err
		}
		if err = s.waitExpiry(o); err != nil {
			return err
		}
		if err = s.hit(i); err != nil {
			return err
		}
		// 关闭已提交但 RPC 未返回时，再阻断回查，让未知期间可稳定观察。
		query, err := set(s.cfg.payment, "payment.before_query", no, "fail", 1000, 0)
		if err != nil {
			return err
		}
		time.Sleep(1200 * time.Millisecond)
		unknown, err := s.order(no, s.buyer)
		if err != nil {
			return err
		}
		e["unknownPeriod"] = unknown
		if str(unknown, "status") != "CLOSING" || str(unknown, "inventoryStatus") != "LOCKED" {
			return fmt.Errorf("关闭结果未知期间提前取消或释放")
		}
		fact, err := s.request(s.cfg.payment, "/api/payments/"+str(p, "paymentNo"), "GET", s.buyer, nil, 200)
		if err != nil {
			return err
		}
		if str(fact, "status") != "CLOSED" {
			return fmt.Errorf("没有触发关闭提交后的响应丢失窗口")
		}
		if err = s.clear(query); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitOrder(no, s.buyer, "CANCELED", "RELEASED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 0, 1, 1, 0, e)
	case 5:
		i, err := set(s.cfg.catalog, "catalog.before_confirm", no, "fail", 1000, 0)
		if err != nil {
			return err
		}
		if err = s.pay(p); err != nil {
			return err
		}
		if err = s.hit(i); err != nil {
			return err
		}
		pending, err := s.waitOrder(no, s.buyer, "PAID", "LOCKED")
		if err != nil {
			return err
		}
		e["waitingForInventory"] = pending
		if pending["fulfillable"] != false {
			return fmt.Errorf("未确认库存却允许履约")
		}
		if err = s.clear(i); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitOrder(no, s.buyer, "PAID", "CONFIRMED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 1, 0, 0, 1, e)
	case 6:
		i, err := set(s.cfg.catalog, "catalog.before_release", no, "fail", 1000, 0)
		if err != nil {
			return err
		}
		if err = s.waitExpiry(o); err != nil {
			return err
		}
		if err = s.hit(i); err != nil {
			return err
		}
		pending, err := s.waitOrder(no, s.buyer, "CANCELED", "LOCKED")
		if err != nil {
			return err
		}
		e["waitingForInventory"] = pending
		if err = s.clear(i); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitOrder(no, s.buyer, "CANCELED", "RELEASED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 0, 1, 1, 0, e)
	case 8:
		// 用另一个真实 SKU 演示释放先于预占；墓碑不能被迟到 Reserve 覆盖。
		sku, err := s.request(s.cfg.catalog, "/api/admin/demo/skus", "POST", s.operator, object{"name": "reorder-" + newID(), "stock": 1}, 201)
		if err != nil {
			return err
		}
		key := object{"skuId": str(sku, "skuId"), "orderNo": "reorder-" + newID()}
		if _, err = s.request(s.cfg.catalog, "/api/admin/demo/inventory/release", "POST", s.operator, key, 200); err != nil {
			return err
		}
		if _, err = s.request(s.cfg.catalog, "/api/admin/demo/inventory/reserve", "POST", s.operator, key, 409); err != nil {
			return err
		}
		if err = s.waitExpiry(o); err != nil {
			return err
		}
		healthyAt = time.Now()
		if _, err = s.waitOrder(no, s.buyer, "CANCELED", "RELEASED"); err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			row, err := s.waitSent(s.cfg.trade, no, "OrderExpiryDue")
			if err != nil {
				return err
			}
			if err = s.replay(s.cfg.trade, row); err != nil {
				return err
			}
			if _, err = s.waitSent(s.cfg.trade, no, "OrderExpiryDue"); err != nil {
				return err
			}
		}
		stock, err := s.request(s.cfg.catalog, "/api/admin/skus/"+str(sku, "skuId")+"/inventory", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		e["releaseBeforeReserveStock"] = stock
		if num(stock, "available") != 1 || num(stock, "locked") != 0 || num(stock, "sold") != 0 {
			return fmt.Errorf("乱序预占改变已释放库存")
		}
		return s.invariant(f, 1, 0, 1, 1, 0, e)
	}
	return fmt.Errorf("未知故障场景")
}

func (s *suite) boots() (map[string]string, error) {
	result := map[string]string{}
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return nil, err
		}
		result[base] = str(d, "bootId")
	}
	return result, nil
}
func (s *suite) awaitRestart(before map[string]string) error {
	return s.await("演示服务退出后恢复就绪", func() (bool, error) {
		changed := false
		for base, boot := range before {
			r, code, err := s.call(base, "/api/admin/diagnostics", "GET", s.operator, nil)
			if err != nil || code != 200 {
				return false, nil
			}
			if str(r.Data, "bootId") != "" && str(r.Data, "bootId") != boot {
				changed = true
			}
		}
		return changed, nil
	})
}
