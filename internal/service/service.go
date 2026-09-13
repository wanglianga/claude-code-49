// Package service 实现县域校车临时改线与学生到站安全服务的全部业务逻辑。
// 方法签名直接使用 proto 生成类型，因此同一个 Service 既能注册为 gRPC 服务，
// 也能被 HTTP JSON 门面直接调用。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "schoolbus/gen/schoolbusv1"
)

// Querier 抽象 pgxpool.Pool 与 pgx.Tx 共有的查询接口。
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service 聚合全部业务方法，直接满足五个 gRPC 服务接口。
type Service struct {
	pb.UnimplementedAdminServiceServer
	pb.UnimplementedTripServiceServer
	pb.UnimplementedRerouteServiceServer
	pb.UnimplementedParentServiceServer
	pb.UnimplementedAuditServiceServer

	db  *pgxpool.Pool
	loc *time.Location
}

// New 创建 Service；loc 用于“今日”与计划发车时间的时区解释。
func New(db *pgxpool.Pool, loc *time.Location) *Service {
	return &Service{db: db, loc: loc}
}

// DB 暴露连接池（seed 使用）。
func (s *Service) DB() *pgxpool.Pool { return s.db }

func (s *Service) withTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// 通用错误
// ---------------------------------------------------------------------------

func errInvalid(msg string) error    { return status.Error(codes.InvalidArgument, msg) }
func errNotFound(msg string) error   { return status.Error(codes.NotFound, msg) }
func errPrecond(msg string) error    { return status.Error(codes.FailedPrecondition, msg) }
func errPermission(msg string) error { return status.Error(codes.PermissionDenied, msg) }

// ---------------------------------------------------------------------------
// 枚举 <-> 数据库文本 映射
// ---------------------------------------------------------------------------

func dirToDB(d pb.Direction) string {
	switch d {
	case pb.Direction_DIRECTION_MORNING_PICKUP:
		return "morning_pickup"
	case pb.Direction_DIRECTION_AFTERNOON_DROPOFF:
		return "afternoon_dropoff"
	}
	return ""
}

func dirFromDB(s string) pb.Direction {
	switch s {
	case "morning_pickup":
		return pb.Direction_DIRECTION_MORNING_PICKUP
	case "afternoon_dropoff":
		return pb.Direction_DIRECTION_AFTERNOON_DROPOFF
	}
	return pb.Direction_DIRECTION_UNSPECIFIED
}

func dirLabel(d pb.Direction) string {
	if d == pb.Direction_DIRECTION_MORNING_PICKUP {
		return "早接"
	}
	return "晚送"
}

func tripStatusFromDB(s string) pb.TripStatus {
	switch s {
	case "scheduled":
		return pb.TripStatus_TRIP_STATUS_SCHEDULED
	case "preparing":
		return pb.TripStatus_TRIP_STATUS_PREPARING
	case "ready":
		return pb.TripStatus_TRIP_STATUS_READY
	case "in_progress":
		return pb.TripStatus_TRIP_STATUS_IN_PROGRESS
	case "completed":
		return pb.TripStatus_TRIP_STATUS_COMPLETED
	case "closed":
		return pb.TripStatus_TRIP_STATUS_CLOSED
	}
	return pb.TripStatus_TRIP_STATUS_UNSPECIFIED
}

func tsStatusFromDB(s string) pb.TripStudentStatus {
	switch s {
	case "expected":
		return pb.TripStudentStatus_TS_EXPECTED
	case "on_leave":
		return pb.TripStudentStatus_TS_ON_LEAVE
	case "self_pickup":
		return pb.TripStudentStatus_TS_SELF_PICKUP
	case "club_activity":
		return pb.TripStudentStatus_TS_CLUB_ACTIVITY
	case "boarded":
		return pb.TripStudentStatus_TS_BOARDED
	case "alighted":
		return pb.TripStudentStatus_TS_ALIGHTED
	case "absent":
		return pb.TripStudentStatus_TS_ABSENT
	case "skipped":
		return pb.TripStudentStatus_TS_SKIPPED
	case "wrong_bus":
		return pb.TripStudentStatus_TS_WRONG_BUS
	case "no_receiver":
		return pb.TripStudentStatus_TS_NO_RECEIVER
	}
	return pb.TripStudentStatus_TRIP_STUDENT_STATUS_UNSPECIFIED
}

func tsStatusToDB(st pb.TripStudentStatus) string {
	switch st {
	case pb.TripStudentStatus_TS_ON_LEAVE:
		return "on_leave"
	case pb.TripStudentStatus_TS_SELF_PICKUP:
		return "self_pickup"
	case pb.TripStudentStatus_TS_CLUB_ACTIVITY:
		return "club_activity"
	case pb.TripStudentStatus_TS_BOARDED:
		return "boarded"
	case pb.TripStudentStatus_TS_ALIGHTED:
		return "alighted"
	case pb.TripStudentStatus_TS_ABSENT:
		return "absent"
	case pb.TripStudentStatus_TS_SKIPPED:
		return "skipped"
	case pb.TripStudentStatus_TS_WRONG_BUS:
		return "wrong_bus"
	case pb.TripStudentStatus_TS_NO_RECEIVER:
		return "no_receiver"
	}
	return "expected"
}

func rerouteReasonToDB(r pb.RerouteReason) string {
	switch r {
	case pb.RerouteReason_REROUTE_ROAD_CONSTRUCTION:
		return "road_construction"
	case pb.RerouteReason_REROUTE_HEAVY_RAIN:
		return "heavy_rain"
	case pb.RerouteReason_REROUTE_VEHICLE_BREAKDOWN:
		return "vehicle_breakdown"
	case pb.RerouteReason_REROUTE_ROAD_CLOSURE:
		return "road_closure"
	case pb.RerouteReason_REROUTE_CONGESTION:
		return "congestion"
	}
	return "other"
}

func rerouteReasonFromDB(s string) pb.RerouteReason {
	switch s {
	case "road_construction":
		return pb.RerouteReason_REROUTE_ROAD_CONSTRUCTION
	case "heavy_rain":
		return pb.RerouteReason_REROUTE_HEAVY_RAIN
	case "vehicle_breakdown":
		return pb.RerouteReason_REROUTE_VEHICLE_BREAKDOWN
	case "road_closure":
		return pb.RerouteReason_REROUTE_ROAD_CLOSURE
	case "congestion":
		return pb.RerouteReason_REROUTE_CONGESTION
	}
	return pb.RerouteReason_REROUTE_OTHER
}

// RerouteReasonLabel 改线原因中文标签（通知文案用）。
func RerouteReasonLabel(r pb.RerouteReason) string {
	switch r {
	case pb.RerouteReason_REROUTE_ROAD_CONSTRUCTION:
		return "道路施工"
	case pb.RerouteReason_REROUTE_HEAVY_RAIN:
		return "暴雨"
	case pb.RerouteReason_REROUTE_VEHICLE_BREAKDOWN:
		return "车辆故障"
	case pb.RerouteReason_REROUTE_ROAD_CLOSURE:
		return "临时封路"
	case pb.RerouteReason_REROUTE_CONGESTION:
		return "站点附近拥堵"
	}
	return "其他原因"
}

func exTypeToDB(t pb.ExceptionType) string {
	switch t {
	case pb.ExceptionType_EX_VEHICLE_OFFLINE:
		return "vehicle_offline"
	case pb.ExceptionType_EX_ESCORT_PHONE_DEAD:
		return "escort_phone_dead"
	case pb.ExceptionType_EX_GUARDIAN_SWAP:
		return "guardian_swap"
	case pb.ExceptionType_EX_NO_SIGNAL:
		return "no_signal"
	case pb.ExceptionType_EX_CLASS_RESCHEDULED:
		return "class_rescheduled"
	case pb.ExceptionType_EX_WRONG_BUS:
		return "wrong_bus"
	case pb.ExceptionType_EX_NO_RECEIVER:
		return "no_receiver"
	}
	return "other"
}

func exTypeFromDB(s string) pb.ExceptionType {
	switch s {
	case "vehicle_offline":
		return pb.ExceptionType_EX_VEHICLE_OFFLINE
	case "escort_phone_dead":
		return pb.ExceptionType_EX_ESCORT_PHONE_DEAD
	case "guardian_swap":
		return pb.ExceptionType_EX_GUARDIAN_SWAP
	case "no_signal":
		return pb.ExceptionType_EX_NO_SIGNAL
	case "class_rescheduled":
		return pb.ExceptionType_EX_CLASS_RESCHEDULED
	case "wrong_bus":
		return pb.ExceptionType_EX_WRONG_BUS
	case "no_receiver":
		return pb.ExceptionType_EX_NO_RECEIVER
	}
	return pb.ExceptionType_EX_OTHER
}

func hmStatusToDB(st pb.HomeroomStatus) string {
	switch st {
	case pb.HomeroomStatus_HM_SAFE_AT_SCHOOL:
		return "safe_at_school"
	case pb.HomeroomStatus_HM_SAFE_AT_HOME:
		return "safe_at_home"
	case pb.HomeroomStatus_HM_EXCEPTION:
		return "exception"
	}
	return ""
}

func hmStatusFromDB(s string) pb.HomeroomStatus {
	switch s {
	case "safe_at_school":
		return pb.HomeroomStatus_HM_SAFE_AT_SCHOOL
	case "safe_at_home":
		return pb.HomeroomStatus_HM_SAFE_AT_HOME
	case "exception":
		return pb.HomeroomStatus_HM_EXCEPTION
	}
	return pb.HomeroomStatus_HOMEROOM_STATUS_UNSPECIFIED
}

// ---------------------------------------------------------------------------
// 时间辅助
// ---------------------------------------------------------------------------

func (s *Service) now() time.Time { return time.Now().In(s.loc) }

func (s *Service) today() string { return s.now().Format("2006-01-02") }

func (s *Service) combineDateTime(date, hhmm string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, s.loc)
	if err != nil {
		return time.Time{}, errInvalid(fmt.Sprintf("日期/时间格式错误: %s %s", date, hhmm))
	}
	return t, nil
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}

func tsVal(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// requireGuardian 校验手机号是否为该学生登记的监护人（家长端身份绑定）。
func (s *Service) requireGuardian(ctx context.Context, q Querier, studentID int64, phone string) error {
	if phone == "" {
		return errPermission("需提供学生监护人的手机号")
	}
	var n int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM guardians WHERE student_id=$1 AND phone=$2`,
		studentID, phone).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errPermission(fmt.Sprintf("号码 %s 不是该学生的登记监护人，无权操作", phone))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 事件与通知
// ---------------------------------------------------------------------------

// 事件类型常量：所有上车、下车、缺乘、改线、电话联系、异常处置都回到同一趟车。
const (
	EvTripGenerated      = "trip_generated"
	EvVehicleConfirmed   = "vehicle_confirmed"
	EvRosterConfirmed    = "roster_confirmed"
	EvTripStarted        = "trip_started"
	EvStopArrived        = "stop_arrived"
	EvStudentBoarded     = "student_boarded"
	EvStudentAlighted    = "student_alighted"
	EvStudentAbsent      = "student_absent"
	EvParentContacted    = "parent_contacted"
	EvWaitUpdated        = "wait_updated"
	EvRerouteInitiated   = "reroute_initiated"
	EvEtaUpdated         = "eta_updated"
	EvFuelUpdated        = "fuel_updated"
	EvExceptionReported  = "exception_reported"
	EvExceptionResolved  = "exception_resolved"
	EvLeaveReported      = "leave_reported"
	EvSelfPickup         = "self_pickup"
	EvClubActivity       = "club_activity"
	EvSiblingRide        = "sibling_ride"
	EvGuardianSwapped    = "guardian_swapped"
	EvClassRescheduled   = "class_rescheduled"
	EvOfflineSync        = "offline_sync"
	EvHomeroomConfirmed  = "homeroom_confirmed"
	EvTripCompleted      = "trip_completed"
	EvTripClosed         = "trip_closed"
	EvNotificationSent   = "notification_sent"
	EvWrongBus           = "wrong_bus"
	EvNoReceiver         = "no_receiver"
	EvSkipStopAuthorized = "skip_stop_authorized"
)

type eventOpts struct {
	studentID  *int64
	stopID     *int64
	actor      string
	detail     map[string]any
	offline    bool
	occurredAt time.Time // 零值表示 now
	synced     bool
}

// emitEvent 向统一事件轨迹追加一条记录，返回事件 ID。
func (s *Service) emitEvent(ctx context.Context, q Querier, tripID int64, etype string, o eventOpts) (int64, error) {
	detail := o.detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	at := o.occurredAt
	if at.IsZero() {
		at = s.now()
	}
	var syncedAt *time.Time
	if o.synced {
		t := s.now()
		syncedAt = &t
	}
	var id int64
	err = q.QueryRow(ctx, `
		INSERT INTO trip_events (trip_id, event_type, student_id, stop_id, actor, detail, offline, occurred_at, synced_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		tripID, etype, o.studentID, o.stopID, o.actor, raw, o.offline, at, syncedAt).Scan(&id)
	return id, err
}

// notifyStudent 给学生的全部监护人写通知，返回创建的通知。
func (s *Service) notifyStudent(ctx context.Context, q Querier, tripID, studentID int64, ntype, content string) ([]*pb.Notification, error) {
	rows, err := q.Query(ctx, `SELECT phone FROM guardians WHERE student_id=$1`, studentID)
	if err != nil {
		return nil, err
	}
	// 先取完再写，避免同一连接上边遍历边写入（conn busy）
	var phones []string
	for rows.Next() {
		var phone string
		if err := rows.Scan(&phone); err != nil {
			rows.Close()
			return nil, err
		}
		phones = append(phones, phone)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*pb.Notification
	for _, phone := range phones {
		n, err := s.insertNotification(ctx, q, tripID, studentID, phone, ntype, content)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Service) insertNotification(ctx context.Context, q Querier, tripID, studentID int64, phone, ntype, content string) (*pb.Notification, error) {
	var id int64
	var createdAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO notifications (trip_id, student_id, phone, ntype, content)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		tripID, studentID, phone, ntype, content).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}
	return &pb.Notification{
		Id: id, TripId: tripID, StudentId: studentID, Phone: phone,
		Ntype: ntype, Content: content, Status: "sent", CreatedAt: tsVal(createdAt),
	}, nil
}

// notifyHomeroomOfStudent 给学生所在班的班主任写通知（学校点名/介入场景）。
func (s *Service) notifyHomeroomOfStudent(ctx context.Context, q Querier, tripID, studentID int64, ntype, content string) error {
	var phone string
	err := q.QueryRow(ctx, `
		SELECT c.homeroom_phone FROM students st JOIN classes c ON c.id=st.class_id
		WHERE st.id=$1 AND c.homeroom_phone <> ''`, studentID).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.insertNotification(ctx, q, tripID, studentID, phone, ntype, content)
	return err
}

// ---------------------------------------------------------------------------
// 读取辅助
// ---------------------------------------------------------------------------

const tripSelect = `
	SELECT t.id, t.schedule_id, t.route_id, r.name, t.vehicle_id, v.plate_no,
	       t.driver_id, d.name, t.escort_id, e.name,
	       to_char(t.service_date,'YYYY-MM-DD'), t.direction, t.status, t.planned_departure,
	       t.vehicle_confirmed_at, t.roster_confirmed_at, t.actual_departure, t.actual_arrival,
	       t.current_seq, t.offline_mode, t.delay_min
	FROM trips t
	JOIN routes r   ON r.id = t.route_id
	JOIN vehicles v ON v.id = t.vehicle_id
	JOIN drivers d  ON d.id = t.driver_id
	JOIN escorts e  ON e.id = t.escort_id`

func scanTrip(row pgx.Row) (*pb.Trip, error) {
	var t pb.Trip
	var dir, st string
	var vc, rc, ad, aa *time.Time
	err := row.Scan(&t.Id, &t.ScheduleId, &t.RouteId, &t.RouteName, &t.VehicleId, &t.VehiclePlate,
		&t.DriverId, &t.DriverName, &t.EscortId, &t.EscortName,
		&t.ServiceDate, &dir, &st, &t.PlannedDeparture,
		&vc, &rc, &ad, &aa, &t.CurrentSeq, &t.OfflineMode, &t.DelayMin)
	if err != nil {
		return nil, err
	}
	t.Direction = dirFromDB(dir)
	t.Status = tripStatusFromDB(st)
	t.VehicleConfirmedAt = tsPtr(vc)
	t.RosterConfirmedAt = tsPtr(rc)
	t.ActualDeparture = tsPtr(ad)
	t.ActualArrival = tsPtr(aa)
	return &t, nil
}

func (s *Service) getTrip(ctx context.Context, q Querier, tripID int64) (*pb.Trip, error) {
	t, err := scanTrip(q.QueryRow(ctx, tripSelect+` WHERE t.id=$1`, tripID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound(fmt.Sprintf("接送任务 %d 不存在", tripID))
	}
	return t, err
}

func (s *Service) loadTripStudents(ctx context.Context, q Querier, tripID int64) ([]*pb.TripStudent, error) {
	rows, err := q.Query(ctx, `
		SELECT ts.id, ts.trip_id, ts.student_id, st.name, c.name, ts.stop_id, sp.name,
		       ts.status, ts.boarded_at, ts.alighted_at, ts.note, st.family_id
		FROM trip_students ts
		JOIN students st ON st.id = ts.student_id
		JOIN classes c   ON c.id = st.class_id
		JOIN stops sp    ON sp.id = ts.stop_id
		WHERE ts.trip_id=$1 ORDER BY ts.id`, tripID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.TripStudent
	for rows.Next() {
		var x pb.TripStudent
		var st string
		var boarded, alighted *time.Time
		if err := rows.Scan(&x.Id, &x.TripId, &x.StudentId, &x.StudentName, &x.ClassName,
			&x.StopId, &x.StopName, &st, &boarded, &alighted, &x.Note, &x.FamilyId); err != nil {
			return nil, err
		}
		x.Status = tsStatusFromDB(st)
		x.BoardedAt = tsPtr(boarded)
		x.AlightedAt = tsPtr(alighted)
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Service) loadStopEtas(ctx context.Context, q Querier, tripID int64) ([]*pb.StopEta, error) {
	rows, err := q.Query(ctx, `
		SELECT e.stop_id, sp.name, e.seq, e.planned_eta, e.current_eta, e.status
		FROM stop_etas e JOIN stops sp ON sp.id=e.stop_id
		WHERE e.trip_id=$1 ORDER BY e.seq`, tripID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.StopEta
	for rows.Next() {
		var e pb.StopEta
		var planned, current *time.Time
		if err := rows.Scan(&e.StopId, &e.StopName, &e.Seq, &planned, &current, &e.Status); err != nil {
			return nil, err
		}
		e.PlannedEta = tsPtr(planned)
		e.CurrentEta = tsPtr(current)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Service) loadOpenExceptions(ctx context.Context, q Querier, tripID int64) ([]*pb.Exception, error) {
	rows, err := q.Query(ctx, `
		SELECT id, trip_id, etype, detail, status, COALESCE(student_id,0), reported_by,
		       resolution, resolved_by, created_at, resolved_at
		FROM exceptions WHERE trip_id=$1 AND status='open' ORDER BY id`, tripID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.Exception
	for rows.Next() {
		var e pb.Exception
		var etype, exStatus string
		var resolvedAt *time.Time
		var createdAt time.Time
		if err := rows.Scan(&e.Id, &e.TripId, &etype, &e.Detail, &exStatus, &e.StudentId,
			&e.ReportedBy, &e.Resolution, &e.ResolvedBy, &createdAt, &resolvedAt); err != nil {
			return nil, err
		}
		e.Type = exTypeFromDB(etype)
		if exStatus == "open" {
			e.Status = pb.ExceptionStatus_EX_STATUS_OPEN
		} else {
			e.Status = pb.ExceptionStatus_EX_STATUS_RESOLVED
		}
		e.CreatedAt = tsVal(createdAt)
		e.ResolvedAt = tsPtr(resolvedAt)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// structFromMap 将 map 转为 protobuf Struct（事件 detail）。
// 先经 JSON 归一化，使 []int64 等类型也能安全转换。
func structFromMap(m map[string]any) *structpb.Struct {
	if m == nil {
		m = map[string]any{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return &structpb.Struct{}
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return &structpb.Struct{}
	}
	st, err := structpb.NewStruct(norm)
	if err != nil {
		return &structpb.Struct{}
	}
	return st
}
