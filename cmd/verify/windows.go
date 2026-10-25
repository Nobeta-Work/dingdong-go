package main

import (
	"fmt"
	"time"
)

// windowCase 验证四个跨系统提交窗口，保留实际故障命中和数据库派生事实。
func (s *suite) windowCase(kind int, e object) (resultErr error) {
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
				resultErr = err
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
	if kind == 3 {
		no := "race-" + newID()
		expires := time.Now().Add(2 * time.Second)
		input := object{"orderNo": no, "userId": s.buyerID, "amountCent": 1990, "expiresAtUnixMilli": expires.UnixMilli()}
		start := make(chan struct{})
		out := make(chan object, 2)
		for _, action := range []string{"create", "close"} {
			go func(action string) {
				<-start
				r, code, err := s.call(s.cfg.payment, "/api/admin/demo/payments/"+action, "POST", s.operator, input)
				v := object{"action": action, "status": code, "data": r.Data}
				if err != nil {
					v["error"] = err.Error()
				}
				out <- v
			}(action)
		}
		close(start)
		first, second := <-out, <-out
		e["concurrentResults"] = []object{first, second}
		for _, r := range []object{first, second} {
			if r["error"] != nil {
				return fmt.Errorf("并发支付操作发生网络错误: %v", r)
			}
			code, _ := r["status"].(int)
			if code != 200 && code != 409 {
				return fmt.Errorf("并发支付操作出现非预期状态 %d", code)
			}
		}
		time.Sleep(time.Until(expires) + 100*time.Millisecond)
		closed, err := s.request(s.cfg.payment, "/api/admin/demo/payments/close", "POST", s.operator, input, 200)
		if err != nil {
			return err
		}
		late, err := s.request(s.cfg.payment, "/api/admin/demo/payments/create", "POST", s.operator, input, 200)
		if err != nil {
			return err
		}
		e["closed"] = closed
		e["lateCreate"] = late
		if str(closed, "status") != "CLOSED" || str(late, "status") != "CLOSED" {
			return fmt.Errorf("支付创建覆盖关闭终态")
		}
		return nil
	}
	f, err := s.fixture(1)
	if err != nil {
		return err
	}
	e["fixture"] = f
	if kind == 2 {
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
		i, err := set(s.cfg.payment, "payment.after_outbox_send", no, "fail", 1, 0)
		if err != nil {
			return err
		}
		if err = s.pay(p); err != nil {
			return err
		}
		if err = s.hit(i); err != nil {
			return err
		}
		healthyAt = time.Now()
		row, err := s.waitSent(s.cfg.payment, no, "PaymentSucceeded")
		if err != nil {
			return err
		}
		e["outbox"] = row
		if num(row, "attempts") < 2 {
			return fmt.Errorf("未触发 Broker 已收、本地未标记 SENT 的重投窗口")
		}
		if err = s.duplicateDelivered(row); err != nil {
			return err
		}
		if _, err = s.waitOrder(no, s.buyer, "PAID", "CONFIRMED"); err != nil {
			return err
		}
		return s.invariant(f, 1, 1, 0, 0, 1, e)
	}
	id := newID()
	e["requestId"] = id
	if kind == 1 {
		before, err := s.boots()
		if err != nil {
			return err
		}
		if _, err = set(s.cfg.trade, "trade.after_prehold", id, "crash", 1, 0); err != nil {
			return err
		}
		_, code, callErr := s.call(s.cfg.trade, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": id})
		e["submissionHTTPStatus"] = code
		if callErr == nil && code == 202 {
			return fmt.Errorf("预扣后退出未触发")
		}
		if err = s.awaitRestart(before); err != nil {
			return err
		}
		healthyAt = time.Now()
		var failed object
		if err = s.await("预扣遗留先写持久化失败", func() (bool, error) {
			r, code, err := s.call(s.cfg.peer, "/api/requests/"+id, "GET", s.buyer, nil)
			if err != nil {
				return false, err
			}
			if code == 404 {
				return false, nil
			}
			if code != 200 {
				return false, fmt.Errorf("查询预扣恢复返回 %d", code)
			}
			failed = r.Data
			return str(failed, "status") == "FAILED", nil
		}); err != nil {
			return err
		}
		e["originalResult"] = failed
		if str(failed, "failureReason") != "ADMISSION_NOT_PERSISTED" {
			return fmt.Errorf("未观察到预扣未落库的失败终态")
		}
		if _, err = s.request(s.cfg.trade, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": id}, 200); err != nil {
			return err
		}
	} else if kind == 4 {
		before, err := s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/admission", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		i, err := set(s.cfg.trade, "trade.after_prehold", id, "delay", 1, 2500)
		if err != nil {
			return err
		}
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			_, code, err := s.call(s.cfg.trade, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": id})
			done <- result{code, err}
		}()
		if err = s.hit(i); err != nil {
			return err
		}
		_, code, err := s.call(s.cfg.peer, "/api/admin/activities/"+f.ActivityID+"/rebuild-admission", "POST", s.operator, nil)
		if err != nil {
			return err
		}
		if code != 200 && code != 503 {
			return fmt.Errorf("重建返回 %d", code)
		}
		r := <-done
		healthyAt = time.Now()
		if r.err != nil {
			return r.err
		}
		if r.code != 409 {
			return fmt.Errorf("旧在途请求未被数据库代次栅栏拒绝: %d", r.code)
		}
		failed, err := s.request(s.cfg.trade, "/api/requests/"+id, "GET", s.buyer, nil, 200)
		if err != nil {
			return err
		}
		e["oldRequest"] = failed
		if str(failed, "status") != "FAILED" || str(failed, "failureReason") != "ADMISSION_GENERATION_CHANGED" {
			return fmt.Errorf("旧在途请求失败原因不符")
		}
		if err = s.await("新代次激活", func() (bool, error) {
			a, err := s.request(s.cfg.peer, "/api/admin/activities/"+f.ActivityID+"/admission", "GET", s.operator, nil, 200)
			e["rebuilt"] = a
			return str(a, "epoch") != str(before, "epoch") && str(a, "state") == "ACTIVE" && str(obj(a, "redis"), "ready") == "1", err
		}); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("未知关键窗口")
	}
	if err = s.await("回补预扣与清理索引", func() (bool, error) {
		a, err := s.request(s.cfg.peer, "/api/admin/activities/"+f.ActivityID+"/admission", "GET", s.operator, nil, 200)
		return str(obj(a, "redis"), "remaining") == "1" && num(a, "pendingEntries") == 1 && num(a, "usedQuota") == 0, err
	}); err != nil {
		return err
	}
	o, err := s.created(f)
	if err != nil {
		return err
	}
	p, err := s.payment(o)
	if err != nil {
		return err
	}
	if err = s.pay(p); err != nil {
		return err
	}
	if _, err = s.waitOrder(str(o, "orderNo"), s.buyer, "PAID", "CONFIRMED"); err != nil {
		return err
	}
	return s.invariant(f, 1, 1, 0, 0, 1, e)
}
