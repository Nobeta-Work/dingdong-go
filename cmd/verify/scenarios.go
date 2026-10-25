package main

import (
	"fmt"
	"sync"
	"time"
)

func (s *suite) cacheCase(e object) error {
	f, err := s.fixture(1)
	if err != nil {
		return err
	}
	e["fixture"] = f
	// 先预热再做一次明确失效；后台发布失效任务必须已经完成，否则实验有额外失效窗口。
	if err = s.waitCacheTasks(); err != nil {
		return err
	}
	if _, err = s.request(s.cfg.trade, "/api/activities/"+f.ActivityID, "GET", "", nil, 200); err != nil {
		return err
	}
	if _, err = s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/cache", "DELETE", s.operator, nil, 200); err != nil {
		return err
	}
	before := 0
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		before += num(obj(d, "metrics"), "cache.db_load")
	}
	start := make(chan struct{})
	errs := make(chan error, 100)
	var wg sync.WaitGroup
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(base string) {
				defer wg.Done()
				<-start
				_, err := s.request(base, "/api/activities/"+f.ActivityID, "GET", "", nil, 200)
				errs <- err
			}(base)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	after := 0
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		after += num(obj(d, "metrics"), "cache.db_load")
	}
	e["requestsPerInstance"] = 50
	e["detailDBQueryDelta"] = after - before
	if after-before < 1 || after-before > 2 {
		return fmt.Errorf("双实例详情主查询增量为 %d，期望 1..2", after-before)
	}
	return nil
}

func (s *suite) waitCacheTasks() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		d, err := s.request(s.cfg.trade, "/api/admin/reliability/operation_task?status=PENDING&pageSize=100", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		processing, err := s.request(s.cfg.trade, "/api/admin/reliability/operation_task?status=PROCESSING&pageSize=100", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		found := false
		for _, list := range []object{d, processing} {
			// 不从分页截断的结果宣称全量完成。
			if num(list, "total") > 100 {
				return fmt.Errorf("实验前队列待处理记录超过 100，请先在独立环境收敛")
			}
			for _, v := range items(list) {
				if row, ok := v.(map[string]any); ok && containsAction(str(row, "payload"), "activityId") {
					found = true
				}
			}
		}
		if !found {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("发布后的缓存失效/重建任务未收敛")
}

func (s *suite) benchTokens(n int) ([]string, error) {
	tokens := make([]string, n)
	errs := make(chan error, n)
	jobs := make(chan int, n)
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				token, _, err := s.login(fmt.Sprintf("bench%04d", i+1))
				tokens[i] = token
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return tokens, nil
}

func (s *suite) concurrencyCase(e object) error {
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		if num(d, "activityBurst") < 500 {
			return fmt.Errorf("正确性实验要求 ACTIVITY_RATE_BURST>=500；不能使用 compose.rate.yaml")
		}
	}
	tokens, err := s.benchTokens(500)
	if err != nil {
		return err
	}
	f, err := s.fixture(200)
	if err != nil {
		return err
	}
	e["fixture"] = f
	start := make(chan struct{})
	codes := make(chan int, 500)
	errs := make(chan error, 500)
	var wg sync.WaitGroup
	begin := time.Now()
	for i, token := range tokens {
		base := s.cfg.trade
		if i%2 == 1 {
			base = s.cfg.peer
		}
		wg.Add(1)
		go func(token, base string) {
			defer wg.Done()
			<-start
			_, code, err := s.call(base, "/api/activities/"+f.ActivityID+"/requests", "POST", token, object{"requestId": newID()})
			codes <- code
			if err != nil {
				errs <- err
			}
		}(token, base)
	}
	close(start)
	wg.Wait()
	close(codes)
	close(errs)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	e["httpStatusCounts"] = counts
	e["submissionDurationMs"] = time.Since(begin).Milliseconds()
	e["acceptedRate"] = float64(counts[202]) / 500
	for err := range errs {
		if err != nil {
			return err
		}
	}
	if counts[202] != 200 || counts[409] != 300 {
		return fmt.Errorf("健康且配额充足的实验要求 200 次受理、300 次明确额度拒绝；实际 %v", counts)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := s.request(s.cfg.trade, "/api/admin/activities/"+f.ActivityID+"/stats", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		if num(obj(stats, "requests"), "accepted")+num(obj(stats, "requests"), "processing") == 0 && num(obj(stats, "orders"), "closing") == 0 {
			e["convergenceDurationMs"] = time.Since(begin).Milliseconds()
			canceled := num(obj(stats, "orders"), "canceled")
			return s.invariant(f, 200, 0, canceled, canceled, 0, e)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("仍有受理未在 30 秒内收敛")
}

func (s *suite) rateCase(e object) error {
	for _, base := range []string{s.cfg.trade, s.cfg.peer} {
		d, err := s.request(base, "/api/admin/diagnostics", "GET", s.operator, nil, 200)
		if err != nil {
			return err
		}
		if num(d, "userBurst") != 3 || num(d, "userRate") != 1 || num(d, "activityBurst") != 3 || num(d, "activityRate") != 1 {
			return fmt.Errorf("限流独立实验要求用户桶/活动桶均为容量3、速率1，使用 compose.rate.yaml")
		}
	}
	tokens, err := s.benchTokens(5)
	if err != nil {
		return err
	}
	f, err := s.fixture(10)
	if err != nil {
		return err
	}
	time.Sleep(4 * time.Second) // 确保与先前实验共享的用户桶补满，不直接删除限流键。
	start := time.Now()
	firstID := newID()
	var statuses []int
	for i := 0; i < 4; i++ {
		id := newID()
		if i == 0 {
			id = firstID
		}
		base := s.cfg.trade
		if i%2 == 1 {
			base = s.cfg.peer
		}
		r, code, err := s.call(base, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": id})
		if err != nil {
			return err
		}
		statuses = append(statuses, code)
		if i < 3 && code != 202 && code != 409 {
			return fmt.Errorf("用户突发第 %d 次响应 %d", i+1, code)
		}
		if i == 3 && (code != 429 || r.Message != "USER_RATE_LIMITED") {
			return fmt.Errorf("第四次未被共享用户桶限流")
		}
	}
	if time.Since(start) >= time.Second {
		return fmt.Errorf("突发样本耗时超过补充周期，实验条件无效")
	}
	if _, err = s.request(s.cfg.peer, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": firstID}, 200); err != nil {
		return fmt.Errorf("持久化重放不应被限流: %w", err)
	}
	time.Sleep(1100 * time.Millisecond)
	_, code, err := s.call(s.cfg.peer, "/api/activities/"+f.ActivityID+"/requests", "POST", s.buyer, object{"requestId": newID()})
	if err != nil {
		return err
	}
	if code != 409 {
		return fmt.Errorf("用户桶补充后应到达资格校验，实际 %d", code)
	}
	e["userBucketStatuses"] = statuses
	g, err := s.fixture(10)
	if err != nil {
		return err
	}
	start = time.Now()
	statuses = nil
	for i := 0; i < 4; i++ {
		base := s.cfg.trade
		if i%2 == 1 {
			base = s.cfg.peer
		}
		r, code, err := s.call(base, "/api/activities/"+g.ActivityID+"/requests", "POST", tokens[i], object{"requestId": newID()})
		if err != nil {
			return err
		}
		statuses = append(statuses, code)
		if i < 3 && code != 202 {
			return fmt.Errorf("活动突发第 %d 次未受理: %d", i+1, code)
		}
		if i == 3 && (code != 429 || r.Message != "ACTIVITY_RATE_LIMITED") {
			return fmt.Errorf("第四个独立用户未被共享活动桶限流")
		}
	}
	if time.Since(start) >= time.Second {
		return fmt.Errorf("活动突发耗时超过补充周期，实验条件无效")
	}
	// 第四个用户被活动桶拒绝时已经扣过用户桶；再两次活动拒绝后，第三次应被用户桶拒绝。
	for i := 0; i < 3; i++ {
		r, code, err := s.call(s.cfg.peer, "/api/activities/"+g.ActivityID+"/requests", "POST", tokens[3], object{"requestId": newID()})
		if err != nil {
			return err
		}
		reason := "ACTIVITY_RATE_LIMITED"
		if i == 2 {
			reason = "USER_RATE_LIMITED"
		}
		if code != 429 || r.Message != reason {
			return fmt.Errorf("第二桶拒绝后的用户令牌扣减不符: %d %s", code, r.Message)
		}
	}
	if time.Since(start) >= time.Second {
		return fmt.Errorf("不退款样本跨过补充周期，实验条件无效")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err = s.request(s.cfg.peer, "/api/activities/"+g.ActivityID+"/requests", "POST", tokens[4], object{"requestId": newID()}, 202); err != nil {
		return err
	}
	e["activityBucketStatuses"] = statuses
	return nil
}
