// Command diagnose 分别读取三个服务的库，在内存中比较事实，不执行跨库 SQL 或写入。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type report struct {
	CheckedAt time.Time `json:"checkedAt"`
	Orders    int       `json:"ordersChecked"`
	SKUs      int       `json:"skusChecked"`
	Problems  []string  `json:"problems"`
}

func main() {
	r := report{CheckedAt: time.Now().UTC(), Problems: []string{}}
	if err := run(&r); err != nil {
		r.Problems = append(r.Problems, err.Error())
	}
	_ = json.NewEncoder(os.Stdout).Encode(r)
	if len(r.Problems) > 0 {
		os.Exit(1)
	}
}

func open(key string) (*sql.DB, error) {
	dsn := os.Getenv(key)
	if dsn == "" {
		return nil, fmt.Errorf("缺少 %s", key)
	}
	return sql.Open("mysql", dsn)
}

func run(r *report) error {
	trade, err := open("DIAG_TRADE_DSN")
	if err != nil {
		return err
	}
	defer trade.Close()
	catalog, err := open("DIAG_CATALOG_DSN")
	if err != nil {
		return err
	}
	defer catalog.Close()
	payment, err := open("DIAG_PAYMENT_DSN")
	if err != nil {
		return err
	}
	defer payment.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 在业务静止窗口执行，跨服务查询不具备全局快照隔离，处理中数据可能暂时不一致。
	var cursor uint64
	// # 库存侧：计数守恒与 LOCKED/CONFIRMED 流水分别核对。
	for {
		rows, err := catalog.QueryContext(ctx, "SELECT sku_id,total,available,locked,sold FROM inventory WHERE sku_id>? ORDER BY sku_id LIMIT 100", cursor)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var id uint64
			var total, available, locked, sold int64
			if err = rows.Scan(&id, &total, &available, &locked, &sold); err != nil {
				rows.Close()
				return err
			}
			cursor = id
			n++
			r.SKUs++
			if total != available+locked+sold || available < 0 || locked < 0 || sold < 0 {
				r.Problems = append(r.Problems, fmt.Sprintf("SKU %d 库存守恒失败", id))
			}
			var reserved, confirmed int64
			if err = catalog.QueryRowContext(ctx, "SELECT COALESCE(SUM(IF(status='LOCKED',quantity,0)),0),COALESCE(SUM(IF(status='CONFIRMED',quantity,0)),0) FROM inventory_reservation WHERE sku_id=?", id).Scan(&reserved, &confirmed); err != nil {
				rows.Close()
				return err
			}
			if reserved != locked || confirmed != sold {
				r.Problems = append(r.Problems, fmt.Sprintf("SKU %d 库存计数与预占流水不一致", id))
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	cursor = 0
	// # 交易侧：按订单游标分页，再分别读取支付库和商品库，避免跨服务 JOIN。
	for {
		rows, err := trade.QueryContext(ctx, `SELECT o.id,o.order_no,o.status,o.inventory_status,o.user_id,o.amount_cent,i.sku_id
			FROM orders o JOIN order_items i ON i.order_no=o.order_no WHERE o.id>? ORDER BY o.id LIMIT 100`, cursor)
		if err != nil {
			return err
		}
		type item struct {
			id                              uint64
			no, state, inventory, user, sku string
			amount                          int64
		}
		batch := make([]item, 0, 100)
		for rows.Next() {
			var o item
			if err = rows.Scan(&o.id, &o.no, &o.state, &o.inventory, &o.user, &o.amount, &o.sku); err != nil {
				break
			}
			batch = append(batch, o)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, o := range batch {
			cursor = o.id
			r.Orders++
			var payState, payUser, reservation string
			var amount int64
			err = payment.QueryRowContext(ctx, "SELECT status,COALESCE(CAST(user_id AS CHAR),''),COALESCE(amount_cent,0) FROM payment WHERE order_no=?", o.no).Scan(&payState, &payUser, &amount)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err = catalog.QueryRowContext(ctx, "SELECT status FROM inventory_reservation WHERE order_no=? AND sku_id=?", o.no, o.sku).Scan(&reservation); err != nil {
				return err
			}
			ok := true
			switch o.state {
			case "PAID":
				ok = payState == "SUCCESS" && payUser == o.user && amount == o.amount && reservation == "CONFIRMED" && o.inventory == "CONFIRMED"
			case "CANCELED":
				ok = payState == "CLOSED" && reservation == "RELEASED" && o.inventory == "RELEASED"
			default:
				ok = reservation == "LOCKED" && o.inventory == "LOCKED" && (payState == "" || payState == "PENDING")
			}
			if !ok {
				r.Problems = append(r.Problems, fmt.Sprintf("订单 %s 未收敛: order=%s inventory=%s payment=%s reservation=%s", o.no, o.state, o.inventory, payState, reservation))
			}
		}
	}
	var bad int
	err = trade.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT activity_id,user_id FROM orders GROUP BY activity_id,user_id HAVING COUNT(*)>1) duplicate_orders`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, "存在同一用户同一活动多张订单")
	}
	err = trade.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders o LEFT JOIN order_items i ON i.order_no=o.order_no
		WHERE i.id IS NULL OR i.quantity<>1 OR o.amount_cent<>i.price_cent*i.quantity`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, "存在缺失或金额不一致的订单快照")
	}
	err = trade.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders o LEFT JOIN activity_request r ON r.order_no=o.order_no
		WHERE r.id IS NULL OR r.status<>'ORDER_CREATED' OR r.user_id<>o.user_id OR r.activity_id<>o.activity_id`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, "订单与受理记录不一致")
	}
	err = trade.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders o LEFT JOIN activity_participation p ON p.activity_id=o.activity_id AND p.user_id=o.user_id
		WHERE p.status IS NULL OR p.status<>'CONSUMED'`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, "成单用户存在未消耗的活动资格")
	}
	err = trade.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity a WHERE a.used_quota<>(SELECT COUNT(*) FROM activity_request r WHERE r.activity_id=a.id AND r.status<>'FAILED')`).Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, "活动额度计数与持久化受理不一致")
	}
	err = trade.QueryRowContext(ctx, "SELECT COUNT(*) FROM activity_request WHERE status IN ('ACCEPTED','PROCESSING')").Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("仍有 %d 条受理未收敛", bad))
	}
	err = trade.QueryRowContext(ctx, "SELECT COUNT(*) FROM operation_task WHERE status<>'DONE'").Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("仍有 %d 条恢复任务未完成", bad))
	}
	err = trade.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE status='CLOSING' OR (status='PENDING_PAYMENT' AND expires_at<=UTC_TIMESTAMP(3))").Scan(&bad)
	if err != nil {
		return err
	}
	if bad > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("仍有 %d 张到期或关闭中订单未收敛", bad))
	}
	return nil
}
