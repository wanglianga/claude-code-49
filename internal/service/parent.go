package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	pb "schoolbus/gen/schoolbusv1"
)

// ---------------------------------------------------------------------------
// ParentService：家长端（预计到站时间、通知、自接/换人/社团）
// ---------------------------------------------------------------------------

// findStudentTrip 查找学生当日的乘车任务（可按方向过滤，默认取进行中的，其次最新的）。
func (s *Service) findStudentTrip(ctx context.Context, q Querier, studentID int64, date string, dir pb.Direction) (int64, error) {
	if date == "" {
		date = s.today()
	}
	sql := `
		SELECT t.id FROM trip_students ts JOIN trips t ON t.id=ts.trip_id
		WHERE ts.student_id=$1 AND t.service_date=$2`
	args := []any{studentID, date}
	if dir != pb.Direction_DIRECTION_UNSPECIFIED {
		sql += ` AND t.direction=$3`
		args = append(args, dirToDB(dir))
	}
	sql += ` ORDER BY (t.status='in_progress') DESC, t.id DESC LIMIT 1`
	var tripID int64
	err := q.QueryRow(ctx, sql, args...).Scan(&tripID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound(fmt.Sprintf("学生 %d 在 %s 没有乘车任务", studentID, date))
	}
	return tripID, err
}

func (s *Service) GetStudentTripStatus(ctx context.Context, req *pb.StudentTripStatusRequest) (*pb.StudentTripStatus, error) {
	var stu pb.Student
	err := s.db.QueryRow(ctx, `
		SELECT st.id, st.class_id, c.name, st.name, st.student_no, st.gender, st.family_id, st.status
		FROM students st JOIN classes c ON c.id=st.class_id WHERE st.id=$1`, req.StudentId).
		Scan(&stu.Id, &stu.ClassId, &stu.ClassName, &stu.Name, &stu.StudentNo, &stu.Gender, &stu.FamilyId, &stu.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("学生不存在")
	}
	if err != nil {
		return nil, err
	}
	tripID, err := s.findStudentTrip(ctx, s.db, req.StudentId, req.Date, req.Direction)
	if err != nil {
		return nil, err
	}
	t, err := s.getTrip(ctx, s.db, tripID)
	if err != nil {
		return nil, err
	}
	ts, err := s.getTripStudent(ctx, s.db, tripID, req.StudentId)
	if err != nil {
		return nil, err
	}
	resp := &pb.StudentTripStatus{Student: &stu, Trip: t, TripStudent: ts}

	// 该学生站点的预计到站时间
	var planned, current *time.Time
	var etaStatus string
	err = s.db.QueryRow(ctx, `
		SELECT planned_eta, current_eta, status FROM stop_etas WHERE trip_id=$1 AND stop_id=$2`,
		tripID, ts.StopId).Scan(&planned, &current, &etaStatus)
	if err == nil {
		var stopName string
		_ = s.db.QueryRow(ctx, `SELECT name FROM stops WHERE id=$1`, ts.StopId).Scan(&stopName)
		resp.StopEta = &pb.StopEta{
			StopId: ts.StopId, StopName: stopName,
			PlannedEta: tsPtr(planned), CurrentEta: tsPtr(current), Status: etaStatus,
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	_ = s.db.QueryRow(ctx, `SELECT phone FROM escorts WHERE id=$1`, t.EscortId).Scan(&resp.EscortPhone)

	// 该学生最近的事件
	rows, err := s.db.Query(ctx, `
		SELECT e.id, e.trip_id, e.event_type, COALESCE(e.student_id,0), COALESCE(st.name,''),
		       COALESCE(e.stop_id,0), COALESCE(sp.name,''), e.actor, e.offline, e.occurred_at
		FROM trip_events e
		LEFT JOIN students st ON st.id=e.student_id
		LEFT JOIN stops sp ON sp.id=e.stop_id
		WHERE e.trip_id=$1 AND (e.student_id=$2 OR e.event_type IN ('trip_started','reroute_initiated','eta_updated','class_rescheduled'))
		ORDER BY e.id DESC LIMIT 10`, tripID, req.StudentId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e pb.TripEvent
		var occurredAt time.Time
		if err := rows.Scan(&e.Id, &e.TripId, &e.EventType, &e.StudentId, &e.StudentName,
			&e.StopId, &e.StopName, &e.Actor, &e.Offline, &occurredAt); err != nil {
			return nil, err
		}
		e.OccurredAt = tsVal(occurredAt)
		resp.RecentEvents = append(resp.RecentEvents, &e)
	}
	return resp, rows.Err()
}

func (s *Service) ListNotifications(ctx context.Context, req *pb.ListNotificationsRequest) (*pb.ListNotificationsResponse, error) {
	if req.Phone == "" {
		return nil, errInvalid("phone 不能为空")
	}
	sql := `SELECT id, COALESCE(trip_id,0), COALESCE(student_id,0), phone, ntype, content, status, created_at
	        FROM notifications WHERE phone=$1`
	args := []any{req.Phone}
	if req.Date != "" {
		sql += ` AND created_at::date = $2`
		args = append(args, req.Date)
	}
	sql += ` ORDER BY id DESC LIMIT 100`
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &pb.ListNotificationsResponse{}
	for rows.Next() {
		var n pb.Notification
		var createdAt time.Time
		if err := rows.Scan(&n.Id, &n.TripId, &n.StudentId, &n.Phone, &n.Ntype, &n.Content, &n.Status, &createdAt); err != nil {
			return nil, err
		}
		n.CreatedAt = tsVal(createdAt)
		out.Notifications = append(out.Notifications, &n)
	}
	return out, rows.Err()
}

// RequestSelfPickup 家长改为自接（下午放学场景）。
func (s *Service) RequestSelfPickup(ctx context.Context, req *pb.SelfPickupRequest) (*pb.TripStudent, error) {
	// 家长身份绑定：仅该学生登记监护人可改为自接
	if err := s.requireGuardian(ctx, s.db, req.StudentId, req.GuardianPhone); err != nil {
		return nil, err
	}
	date := req.Date
	if date == "" {
		date = s.today()
	}
	tripID, err := s.findStudentTrip(ctx, s.db, req.StudentId, date, pb.Direction_DIRECTION_AFTERNOON_DROPOFF)
	if err != nil {
		// 没有晚送任务时退回当日任意任务
		tripID, err = s.findStudentTrip(ctx, s.db, req.StudentId, date, pb.Direction_DIRECTION_UNSPECIFIED)
		if err != nil {
			return nil, err
		}
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='self_pickup', note=$3
			WHERE trip_id=$1 AND student_id=$2 AND status IN ('expected','absent')`,
			tripID, req.StudentId, req.Note)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errPrecond("学生当前状态不允许改为自接")
		}
		if _, err := s.emitEvent(ctx, tx, tripID, EvSelfPickup, eventOpts{
			studentID: &req.StudentId,
			actor:     "guardian:" + req.GuardianPhone,
			detail:    map[string]any{"note": req.Note},
		}); err != nil {
			return err
		}
		return s.notifyHomeroomOfStudent(ctx, tx, tripID, req.StudentId, "self_pickup",
			"【校车】家长申请今日自接，请班主任知悉。")
	})
	if err != nil {
		return nil, err
	}
	return s.getTripStudent(ctx, s.db, tripID, req.StudentId)
}

// RequestGuardianSwap 家长临时换人接送：生成当日有效的临时授权并留痕。
func (s *Service) RequestGuardianSwap(ctx context.Context, req *pb.GuardianSwapRequest) (*pb.PickupAuthorization, error) {
	if req.TempName == "" {
		return nil, errInvalid("临时接送人姓名不能为空")
	}
	// 家长身份绑定：仅该学生登记监护人可发起换人
	if err := s.requireGuardian(ctx, s.db, req.StudentId, req.GuardianPhone); err != nil {
		return nil, err
	}
	date := req.Date
	if date == "" {
		date = s.today()
	}
	auth := &pb.PickupAuthorization{
		StudentId: req.StudentId, AuthorizedName: req.TempName, AuthorizedPhone: req.TempPhone,
		Relation: req.Relation, AuthType: "temporary", ValidDate: date, CreatedBy: "guardian:" + req.GuardianPhone,
	}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO pickup_authorizations (student_id, authorized_name, authorized_phone, relation, auth_type, valid_date, created_by)
			VALUES ($1,$2,$3,$4,'temporary',$5,$6) RETURNING id`,
			req.StudentId, req.TempName, req.TempPhone, req.Relation, date, auth.CreatedBy).Scan(&auth.Id); err != nil {
			return err
		}
		// 在当日相关任务上留痕
		rows, err := tx.Query(ctx, `
			SELECT t.id FROM trip_students ts JOIN trips t ON t.id=ts.trip_id
			WHERE ts.student_id=$1 AND t.service_date=$2`, req.StudentId, date)
		if err != nil {
			return err
		}
		var tripIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			tripIDs = append(tripIDs, id)
		}
		rows.Close()
		for _, tripID := range tripIDs {
			if _, err := s.insertException(ctx, tx, tripID, pb.ExceptionType_EX_GUARDIAN_SWAP,
				fmt.Sprintf("家长临时换人接送：%s（%s，%s）今日代接，已生成临时授权", req.TempName, req.Relation, req.TempPhone),
				req.StudentId, "guardian:"+req.GuardianPhone); err != nil {
				return err
			}
			if _, err := s.emitEvent(ctx, tx, tripID, EvGuardianSwapped, eventOpts{
				studentID: &req.StudentId,
				actor:     "guardian:" + req.GuardianPhone,
				detail: map[string]any{
					"temp_name": req.TempName, "temp_phone": req.TempPhone,
					"relation": req.Relation, "valid_date": date,
				},
			}); err != nil {
				return err
			}
			if err := s.notifyHomeroomOfStudent(ctx, tx, tripID, req.StudentId, "guardian_swap",
				fmt.Sprintf("【校车】家长临时换人接送：今日由 %s（%s）代接，请知悉。", req.TempName, req.Relation)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return auth, nil
}

// ReportClubActivity 学生临时参加社团，不乘晚班车。
func (s *Service) ReportClubActivity(ctx context.Context, req *pb.ClubActivityRequest) (*pb.TripStudent, error) {
	if req.ClubName == "" {
		return nil, errInvalid("社团名称不能为空")
	}
	date := req.Date
	if date == "" {
		date = s.today()
	}
	tripID, err := s.findStudentTrip(ctx, s.db, req.StudentId, date, pb.Direction_DIRECTION_AFTERNOON_DROPOFF)
	if err != nil {
		return nil, err
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='club_activity', note=$3
			WHERE trip_id=$1 AND student_id=$2 AND status='expected'`,
			tripID, req.StudentId, "参加社团："+req.ClubName)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errPrecond("学生当前状态不允许登记社团活动")
		}
		if _, err := s.emitEvent(ctx, tx, tripID, EvClubActivity, eventOpts{
			studentID: &req.StudentId,
			actor:     "school",
			detail:    map[string]any{"club_name": req.ClubName},
		}); err != nil {
			return err
		}
		_, err = s.notifyStudent(ctx, tx, tripID, req.StudentId, "club_activity",
			fmt.Sprintf("【校车】%s 今日参加社团「%s」，不乘坐晚班车，请家长安排接回。", studentName(ctx, tx, req.StudentId), req.ClubName))
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getTripStudent(ctx, s.db, tripID, req.StudentId)
}

func studentName(ctx context.Context, q Querier, studentID int64) string {
	var name string
	if err := q.QueryRow(ctx, `SELECT name FROM students WHERE id=$1`, studentID).Scan(&name); err != nil {
		return ""
	}
	return name
}
