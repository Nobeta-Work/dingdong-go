package trade

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	pb "github.com/Nobeta-Work/dingdong-go/api/gen/catalog/v1"
	"strconv"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed lua/*.lua
var luaFiles embed.FS

func script(name string) *redis.Script {
	body, err := luaFiles.ReadFile("lua/" + name + ".lua")
	if err != nil {
		panic(err)
	}
	return redis.NewScript(string(body))
}

var rateScript = script("rate")
var admitScript = script("admit")
var resolveScript = script("resolve")
var prepareScript = script("prepare")
var rebuildScript = script("rebuild")
var activateScript = script("activate")
var pendingScript = script("pending")

// ticket 是 Redis 预扣记录；代次、版本和业务 ID 使用字符串，避免 Lua/JSON 浮点精度损失。
type ticket struct {
	ActivityID string `json:"activityId"`
	UserID     string `json:"userId"`
	RequestID  string `json:"requestId"`
	OrderNo    string `json:"orderNo"`
	Hash       string `json:"parameterHash"`
	Epoch      string `json:"epoch"`
	Version    string `json:"version"`
	CreatedMS  int64  `json:"createdMs"`
	State      string `json:"state"`
}

func admissionKeys(id string) []string {
	prefix := "dingdong:{activity:" + id + "}:"
	return []string{prefix + "config", prefix + "holders", prefix + "records", prefix + "pending"}
}

func (s *service) prehold(ctx context.Context, id, user, requestID, hash string) (ticket, error) {
	t := ticket{ActivityID: id, UserID: user, RequestID: requestID, OrderNo: "O" + uuid.NewString(), Hash: hash}
	body, err := json.Marshal(t)
	if err != nil {
		return ticket{}, err
	}
	result, err := admitScript.Run(ctx, s.redis, admissionKeys(id), string(body)).StringSlice()
	if err != nil {
		return ticket{}, status.Error(codes.Unavailable, "Redis 准入暂不可用")
	}
	if len(result) != 2 {
		return ticket{}, fmt.Errorf("Redis 准入响应格式错误")
	}
	switch result[0] {
	case "OK":
		err = json.Unmarshal([]byte(result[1]), &t)
		return t, err
	case "REBUILD":
		if _, err = s.scheduleRebuild(ctx, id); err != nil {
			return ticket{}, err
		}
		return ticket{}, status.Error(codes.Unavailable, "准入状态重建中，请稍后重试")
	case "QUOTA":
		return ticket{}, status.Error(codes.FailedPrecondition, "ACTIVITY_QUOTA_EXHAUSTED")
	case "HELD":
		return ticket{}, status.Error(codes.AlreadyExists, "该活动已有受理或订单")
	case "CONFLICT":
		return ticket{}, status.Error(codes.AlreadyExists, "相同 requestId 参数不一致")
	default:
		return ticket{}, status.Error(codes.FailedPrecondition, "活动当前不接受新请求")
	}
}

func (s *service) scheduleRebuild(ctx context.Context, id string) (activity, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return activity{}, err
	}
	defer tx.Rollback()
	a, err := getActivity(ctx, tx, id, true)
	if err != nil {
		return activity{}, err
	}
	if a.Status == "DRAFT" {
		return activity{}, status.Error(codes.FailedPrecondition, "草稿活动尚未发布")
	}
	if a.AdmissionState != "RECOVERING" {
		_, err = tx.ExecContext(ctx, "UPDATE activity SET admission_epoch=admission_epoch+1,admission_state='RECOVERING' WHERE id=?", id)
		if err != nil {
			return activity{}, err
		}
		a, err = getActivity(ctx, tx, id, true)
		if err != nil {
			return activity{}, err
		}
	}
	payload := map[string]string{"activityId": id, "epoch": a.AdmissionEpoch}
	if err = work.PutTask(ctx, tx, id+":"+a.AdmissionEpoch, "RebuildAdmission", payload, time.Now(), "rebuild"); err != nil {
		return activity{}, err
	}
	if err = tx.Commit(); err != nil {
		return activity{}, err
	}
	return a, nil
}

func (s *service) metadataReady(ctx context.Context, a activity) bool {
	keys := admissionKeys(a.ID)
	pipe := s.redis.Pipeline()
	conf := pipe.HMGet(ctx, keys[0], "epoch", "ready")
	holders := pipe.HGet(ctx, keys[1], "__epoch__")
	records := pipe.HGet(ctx, keys[2], "__epoch__")
	pending := pipe.ZScore(ctx, keys[3], "__epoch__")
	if _, err := pipe.Exec(ctx); err != nil {
		return false
	}
	values := conf.Val()
	return len(values) == 2 && values[0] == a.AdmissionEpoch && values[1] == "1" && holders.Val() == a.AdmissionEpoch && records.Val() == a.AdmissionEpoch && pending.Err() == nil
}

// buildAdmission 分页提取资格并写入 Redis，最后用代次和修订号验证快照并条件提交。
// builder 令牌还保护 Redis 分页写入：过期重建者不能污染另一个重建者的数据。
func (s *service) buildAdmission(ctx context.Context, id, epoch string) error {
	a, err := getActivity(ctx, s.db, id, false)
	if err != nil {
		return err
	}
	if a.AdmissionEpoch != epoch {
		return nil
	}
	// 后台恢复发布与显式发布遵守同一条件，不能绕过 SKU 库存校验。
	if a.Status == "PUBLISHING" {
		rpcCtx, cancel := context.WithTimeout(ctx, time.Second)
		sku, checkErr := s.catalog.GetSKU(rpcCtx, &pb.GetSKURequest{SkuId: a.SKUID})
		cancel()
		if checkErr != nil {
			return checkErr
		}
		if !sku.Available || int64(a.Quota) > int64(sku.StockAvailable) {
			return status.Error(codes.FailedPrecondition, "活动配额超过 SKU 可售库存或 SKU 未启用")
		}
	}
	if a.AdmissionState == "ACTIVE" {
		if s.metadataReady(ctx, a) {
			return nil
		}
		_, err = s.scheduleRebuild(ctx, id)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err = getActivity(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if a.AdmissionEpoch != epoch || a.AdmissionState != "RECOVERING" {
		return nil
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	owner := uuid.NewString()
	state := a.Status
	if state == "PUBLISHING" {
		state = "PUBLISHED"
	}
	keys := admissionKeys(id)
	prepared, err := prepareScript.Run(ctx, s.redis, keys, epoch, owner, a.Quota-a.UsedQuota, a.Quota, state, a.Version, a.StartsAt.UnixMilli(), a.EndsAt.UnixMilli()).Int()
	if err != nil {
		return err
	}
	if prepared == 2 {
		return nil
	}
	if prepared != 1 {
		return fmt.Errorf("Redis 重建已被其他执行者领取或代次失效")
	}
	// 恢复期间不接受新持有者；资格释放会递增 revision，最后一次条件检查使分页读取具有一致性。
	// * 每页最多 100 条，读完关闭 rows 再写 Redis，不持有数据库事务进行网络调用。
	var cursor uint64
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT p.user_id,p.holder_request_id,r.order_no,r.parameter_hash FROM activity_participation p
			JOIN activity_request r ON r.user_id=p.user_id AND r.request_id=p.holder_request_id
			WHERE p.activity_id=? AND p.user_id>? AND p.status IN ('HELD','CONSUMED') ORDER BY p.user_id LIMIT 100`, id, cursor)
		if err != nil {
			return err
		}
		args := []any{epoch, owner}
		for rows.Next() {
			t := ticket{ActivityID: id, Epoch: epoch, Version: strconv.FormatInt(a.Version, 10), State: "DURABLE"}
			if err = rows.Scan(&t.UserID, &t.RequestID, &t.OrderNo, &t.Hash); err != nil {
				rows.Close()
				return err
			}
			cursor, err = strconv.ParseUint(t.UserID, 10, 64)
			if err != nil {
				rows.Close()
				return err
			}
			body, err := json.Marshal(t)
			if err != nil {
				rows.Close()
				return err
			}
			args = append(args, string(body))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(args) == 2 {
			break
		}
		n, err := rebuildScript.Run(ctx, s.redis, keys, args...).Int()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("Redis 重建令牌已失效")
		}
	}
	final, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer final.Rollback()
	current, err := getActivity(ctx, final, id, true)
	if err != nil {
		return err
	}
	if current.AdmissionEpoch != epoch || current.AdmissionState != "RECOVERING" || current.AdmissionRevision != a.AdmissionRevision {
		return fmt.Errorf("重建快照已变化，稍后按新快照重试")
	}
	_, err = final.ExecContext(ctx, "UPDATE activity SET admission_state='ACTIVE',status=IF(status='PUBLISHING','PUBLISHED',status) WHERE id=?", id)
	if err != nil {
		return err
	}
	if err = s.invalidateTask(ctx, final, id); err != nil {
		return err
	}
	if err = final.Commit(); err != nil {
		return err
	}
	// Redis 激活在事务外；失败后新请求会推进新的数据库代次，旧请求无法越过栅栏。
	n, err := activateScript.Run(ctx, s.redis, keys, epoch, owner).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("准入激活失败，需要新代次重建")
	}
	s.metrics.Add("admission.rebuilt", 1)
	return nil
}

func (s *service) resolveAdmission(ctx context.Context, id, user, requestID, action string) error {
	n, err := resolveScript.Run(ctx, s.redis, admissionKeys(id), user, requestID, action).Int()
	if err != nil {
		return err
	}
	if n < 0 {
		_, err = s.scheduleRebuild(ctx, id)
		return err
	}
	return nil
}

// terminalizeHold 先以用户+requestId 写失败终态，竞争中的迟到受理只能读到该原结果。
func (s *service) terminalizeHold(ctx context.Context, t ticket) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := getActivity(ctx, tx, t.ActivityID, true)
	if err != nil {
		return err
	}
	r, err := readRequest(ctx, tx, t.UserID, t.RequestID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		r = request{ID: t.RequestID, UserID: t.UserID, ActivityID: t.ActivityID, OrderNo: t.OrderNo, Status: "FAILED", FailureReason: "ADMISSION_NOT_PERSISTED", Snapshot: a, ParameterHash: t.Hash}
		if err = s.insertRequest(ctx, tx, r, t.Epoch); err != nil {
			return err
		}
	}
	action := "AcknowledgeAdmission"
	if r.Status == "FAILED" || r.ParameterHash != t.Hash || r.ActivityID != t.ActivityID {
		action = "RefundAdmission"
	}
	if err = work.PutTask(ctx, tx, t.OrderNo, action, map[string]string{"activityId": t.ActivityID, "userId": t.UserID, "requestId": t.RequestID}, time.Now(), "admission-recovery"); err != nil {
		return err
	}
	return tx.Commit()
}
