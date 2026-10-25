package trade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/Nobeta-Work/dingdong-go/internal/platform/event"
	"github.com/Nobeta-Work/dingdong-go/internal/platform/work"
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

func requestParameters(activityID string) (string, string, error) {
	id, err := strconv.ParseUint(activityID, 10, 64)
	if err != nil || id == 0 {
		return "", "", status.Error(codes.InvalidArgument, "活动 ID 不合法")
	}
	canonical := strconv.FormatUint(id, 10)
	sum := sha256.Sum256([]byte(canonical + ":1"))
	return canonical, hex.EncodeToString(sum[:]), nil
}

func (s *service) replay(ctx context.Context, user, requestID, hash string) (request, bool, error) {
	r, err := readRequest(ctx, s.db, user, requestID)
	if err == sql.ErrNoRows {
		return request{}, false, nil
	}
	if err != nil {
		return request{}, false, err
	}
	if r.ParameterHash != hash {
		return request{}, true, status.Error(codes.AlreadyExists, "同一个 requestId 的请求参数不一致")
	}
	return r, true, nil
}

func (s *service) submit(ctx context.Context, activityID, userID, requestID, trace string) (request, bool, error) {
	if !requestIDPattern.MatchString(requestID) {
		return request{}, false, status.Error(codes.InvalidArgument, "requestId 不合法")
	}
	id, hash, err := requestParameters(activityID)
	if err != nil {
		return request{}, false, err
	}
	if r, exists, err := s.replay(ctx, userID, requestID, hash); err != nil || exists {
		return r, false, err
	}
	t, err := s.prehold(ctx, id, userID, requestID, hash)
	if err != nil {
		return request{}, false, err
	}
	s.metrics.Add("admission.preheld", 1)
	// 故障后保留 Redis 索引，由扫描竞争写入失败终态，再可靠回补，不能只靠 TTL 遗忘。
	if err = s.faults.Hit(ctx, "trade.after_prehold", requestID); err != nil {
		return request{}, false, err
	}
	r, created, err := s.persistTicket(ctx, t, trace)
	if err == nil && created {
		if err = s.faults.Hit(ctx, "trade.after_accept_commit", requestID); err != nil {
			return request{}, false, err
		}
	}
	return r, created, err
}

func (s *service) insertRequest(ctx context.Context, tx *sql.Tx, r request, epoch string) error {
	body, err := json.Marshal(r.Snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO activity_request (user_id,request_id,parameter_hash,activity_id,activity_version,snapshot,order_no,status,failure_reason,admission_epoch)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, r.UserID, r.ID, r.ParameterHash, r.ActivityID, r.Snapshot.Version, string(body), r.OrderNo, r.Status, r.FailureReason, epoch)
	return err
}

func (s *service) persistTicket(ctx context.Context, t ticket, trace string) (request, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return request{}, false, err
	}
	defer tx.Rollback()
	a, err := getActivity(ctx, tx, t.ActivityID, true)
	if err != nil {
		return request{}, false, err
	}
	if existing, err := readRequest(ctx, tx, t.UserID, t.RequestID); err == nil {
		return sameRequest(existing, t.Hash)
	} else if err != sql.ErrNoRows {
		return request{}, false, err
	}
	r := request{ID: t.RequestID, UserID: t.UserID, ActivityID: t.ActivityID, OrderNo: t.OrderNo, Status: "ACCEPTED", Snapshot: a, ParameterHash: t.Hash}
	reason := ""
	if a.AdmissionState != "ACTIVE" || a.AdmissionEpoch != t.Epoch || strconv.FormatInt(a.Version, 10) != t.Version {
		reason = "ADMISSION_GENERATION_CHANGED"
		s.metrics.Add("admission.fenced", 1)
	} else if a.Status != "PUBLISHED" || time.Now().Before(a.StartsAt) || !time.Now().Before(a.EndsAt) {
		reason = "ACTIVITY_NOT_ACCEPTING"
	} else if a.UsedQuota >= a.Quota {
		reason = "ACTIVITY_QUOTA_EXHAUSTED"
	}
	if reason == "" {
		_, err = tx.ExecContext(ctx, "INSERT INTO activity_participation (activity_id,user_id,status) VALUES (?,?,'AVAILABLE') ON DUPLICATE KEY UPDATE user_id=user_id", t.ActivityID, t.UserID)
		if err != nil {
			return request{}, false, err
		}
		var qualification string
		if err = tx.QueryRowContext(ctx, "SELECT status FROM activity_participation WHERE activity_id=? AND user_id=? FOR UPDATE", t.ActivityID, t.UserID).Scan(&qualification); err != nil {
			return request{}, false, err
		}
		if qualification != "AVAILABLE" {
			reason = "PARTICIPATION_ALREADY_HELD"
		}
	}
	if reason != "" {
		r.Status = "FAILED"
		r.FailureReason = reason
		if err = s.insertRequest(ctx, tx, r, t.Epoch); err != nil {
			return request{}, false, err
		}
		if err = work.PutTask(ctx, tx, r.OrderNo, "RefundAdmission", map[string]string{"activityId": r.ActivityID, "userId": r.UserID, "requestId": r.ID}, time.Now(), trace); err != nil {
			return request{}, false, err
		}
		if err = tx.Commit(); err != nil {
			return request{}, false, err
		}
		return r, false, status.Error(codes.FailedPrecondition, reason)
	}
	if err = s.insertRequest(ctx, tx, r, t.Epoch); err != nil {
		return request{}, false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity_participation SET status='HELD',holder_request_id=? WHERE activity_id=? AND user_id=?", t.RequestID, t.ActivityID, t.UserID)
	if err != nil {
		return request{}, false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE activity SET used_quota=used_quota+1,admission_revision=admission_revision+1 WHERE id=?", t.ActivityID)
	if err != nil {
		return request{}, false, err
	}
	payload := map[string]string{"orderNo": r.OrderNo}
	if err = event.Put(ctx, tx, "CreateOrderRequested", r.OrderNo, payload, time.Now(), trace); err != nil {
		return request{}, false, err
	}
	// MQ 正常时 Inbox 提前唤醒任务；断链时本地持久化任务十秒后仍可继续，且不重置人工检查状态。
	if err = work.PutTask(ctx, tx, r.OrderNo, "CreateOrderRequested", payload, time.Now().Add(10*time.Second), trace); err != nil {
		return request{}, false, err
	}
	if err = work.PutTask(ctx, tx, r.OrderNo, "AcknowledgeAdmission", map[string]string{"activityId": r.ActivityID, "userId": r.UserID, "requestId": r.ID}, time.Now(), trace); err != nil {
		return request{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return request{}, false, err
	}
	s.metrics.Add("requests.accepted", 1)
	s.logger.Info("request_accepted", "requestId", r.ID, "orderNo", r.OrderNo, "activityId", r.ActivityID, "traceId", trace)
	return r, true, nil
}

func sameRequest(r request, hash string) (request, bool, error) {
	if r.ParameterHash != hash {
		return request{}, false, status.Error(codes.AlreadyExists, "同一个 requestId 的请求参数不一致")
	}
	return r, false, nil
}
