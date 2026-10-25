package trade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func readRequest(ctx context.Context, q rowQuerier, userID, requestID string) (request, error) {
	var r request
	var snapshot []byte
	err := q.QueryRowContext(ctx, `SELECT request_id,user_id,activity_id,order_no,status,failure_reason,parameter_hash,snapshot
		FROM activity_request WHERE user_id=? AND request_id=?`, userID, requestID).Scan(&r.ID, &r.UserID, &r.ActivityID, &r.OrderNo, &r.Status, &r.FailureReason, &r.ParameterHash, &snapshot)
	if err == nil {
		err = json.Unmarshal(snapshot, &r.Snapshot)
	}
	return r, err
}

func (s *service) submit(ctx context.Context, activityID, userID, requestID, trace string) (request, bool, error) {
	if !requestIDPattern.MatchString(requestID) {
		return request{}, false, status.Error(codes.InvalidArgument, "requestId 仅允许 1~64 位字母、数字、下划线或短横线")
	}
	id, err := strconv.ParseUint(activityID, 10, 64)
	if err != nil || id == 0 {
		return request{}, false, status.Error(codes.InvalidArgument, "活动 ID 不合法")
	}
	activityID = strconv.FormatUint(id, 10)
	sum := sha256.Sum256([]byte(activityID + ":1"))
	hash := hex.EncodeToString(sum[:])
	if r, err := readRequest(ctx, s.db, userID, requestID); err == nil {
		return sameRequest(r, hash)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return request{}, false, err
	}
	// Redis 故障时关闭新受理入口；M1 的额度事实仍由 MySQL 本地事务维护。
	redisCtx, redisCancel := context.WithTimeout(ctx, time.Second)
	err = s.redis.Ping(redisCtx).Err()
	redisCancel()
	if err != nil {
		return request{}, false, status.Error(codes.Unavailable, "准入依赖暂不可用")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return request{}, false, err
	}
	defer tx.Rollback()
	// # 持久化受理：活动行锁串行检查额度，资格、请求和成单事件同时提交。
	// M1 使用数据库准入，避免引入尚未实现恢复栅栏的 Redis 预扣窗口。
	a, err := getActivity(ctx, tx, activityID, true)
	if err != nil {
		return request{}, false, err
	}
	if r, err := readRequest(ctx, tx, userID, requestID); err == nil {
		return sameRequest(r, hash)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return request{}, false, err
	}
	now := time.Now().UTC()
	if a.Status != "PUBLISHED" || now.Before(a.StartsAt) || !now.Before(a.EndsAt) {
		return request{}, false, status.Error(codes.FailedPrecondition, "活动当前不接受新请求")
	}
	if a.UsedQuota >= a.Quota {
		return request{}, false, status.Error(codes.FailedPrecondition, "ACTIVITY_QUOTA_EXHAUSTED")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO activity_participation (activity_id,user_id,status) VALUES (?,?,'AVAILABLE') ON DUPLICATE KEY UPDATE user_id=user_id", activityID, userID)
	if err != nil {
		return request{}, false, err
	}
	var qualification string
	err = tx.QueryRowContext(ctx, "SELECT status FROM activity_participation WHERE activity_id=? AND user_id=? FOR UPDATE", activityID, userID).Scan(&qualification)
	if err != nil {
		return request{}, false, err
	}
	if qualification != "AVAILABLE" {
		return request{}, false, status.Error(codes.AlreadyExists, "该活动已有受理或订单，请查询参与状态")
	}
	r := request{ID: requestID, UserID: userID, ActivityID: activityID, OrderNo: "O" + uuid.NewString(), Status: "ACCEPTED", Snapshot: a, ParameterHash: hash}
	snapshot, err := json.Marshal(a)
	if err != nil {
		return request{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity_request (user_id,request_id,parameter_hash,activity_id,activity_version,snapshot,order_no,status)
		VALUES (?,?,?,?,?,?,?,'ACCEPTED')`, userID, requestID, hash, activityID, a.Version, string(snapshot), r.OrderNo)
	if err != nil {
		return request{}, false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity_participation SET status='HELD',holder_request_id=? WHERE activity_id=? AND user_id=?", requestID, activityID, userID)
	if err != nil {
		return request{}, false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity SET used_quota=used_quota+1 WHERE id=?", activityID)
	if err != nil {
		return request{}, false, err
	}
	if err = event.Put(ctx, tx, "CreateOrderRequested", r.OrderNo, map[string]string{"orderNo": r.OrderNo}, now, trace); err != nil {
		return request{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return request{}, false, err
	}
	s.logger.Info("request_accepted", "requestId", r.ID, "orderNo", r.OrderNo, "activityId", r.ActivityID, "traceId", trace)
	return r, true, nil
}

func sameRequest(r request, hash string) (request, bool, error) {
	if r.ParameterHash != hash {
		return request{}, false, status.Error(codes.AlreadyExists, "同一个 requestId 的请求参数不一致")
	}
	return r, false, nil
}
