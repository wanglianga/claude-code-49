package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "schoolbus/gen/schoolbusv1"
)

// ---------------------------------------------------------------------------
// 每日排班 -> 接送任务
// ---------------------------------------------------------------------------

func (s *Service) GenerateTrips(ctx context.Context, req *pb.GenerateTripsRequest) (*pb.GenerateTripsResponse, error) {
	date := req.Date
	if date == "" {
		date = s.today()
	}
	schedules, err := s.db.Query(ctx, `
		SELECT id, route_id, vehicle_id, driver_id, escort_id, direction, planned_departure
		FROM schedules WHERE service_date=$1 ORDER BY id`, date)
	if err != nil {
		return nil, err
	}
	defer schedules.Close()

	type sched struct {
		id, routeID, vehicleID, driverID, escortID int64
		dir, departure                             string
	}
	var list []sched
	for schedules.Next() {
		var sc sched
		if err := schedules.Scan(&sc.id, &sc.routeID, &sc.vehicleID, &sc.driverID, &sc.escortID, &sc.dir, &sc.departure); err != nil {
			return nil, err
		}
		list = append(list, sc)
	}
	if err := schedules.Err(); err != nil {
		return nil, err
	}

	for _, sc := range list {
		// 幂等：同一排班只生成一次任务
		var exists int64
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM trips WHERE schedule_id=$1`, sc.id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists > 0 {
			continue
		}
		if err := s.withTx(ctx, func(tx pgx.Tx) error {
			var tripID int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO trips (schedule_id, route_id, vehicle_id, driver_id, escort_id, service_date, direction, planned_departure)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
				sc.id, sc.routeID, sc.vehicleID, sc.driverID, sc.escortID, date, sc.dir, sc.departure).Scan(&tripID); err != nil {
				return err
			}
			// 按线路分配生成乘车名单；当日请假的学生直接标记 on_leave
			if _, err := tx.Exec(ctx, `
				INSERT INTO trip_students (trip_id, student_id, stop_id, status)
				SELECT $1, sr.student_id, sr.stop_id,
				       CASE WHEN lr.id IS NOT NULL THEN 'on_leave' ELSE 'expected' END
				FROM student_routes sr
				LEFT JOIN leave_records lr ON lr.student_id=sr.student_id AND lr.leave_date=$2
				WHERE sr.route_id=$3 AND sr.direction=$4
				ON CONFLICT (trip_id, student_id) DO NOTHING`,
				tripID, date, sc.routeID, sc.dir); err != nil {
				return err
			}
			// 生成各站计划 ETA
			base, err := s.combineDateTime(date, sc.departure)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT stop_id, seq, offset_min FROM route_stops WHERE route_id=$1`, sc.routeID)
			if err != nil {
				return err
			}
			type rs struct {
				stopID int64
				seq    int
				offset int
			}
			var stops []rs
			for rows.Next() {
				var r rs
				if err := rows.Scan(&r.stopID, &r.seq, &r.offset); err != nil {
					rows.Close()
					return err
				}
				stops = append(stops, r)
			}
			rows.Close()
			for _, r := range stops {
				eta := base.Add(time.Duration(r.offset) * time.Minute)
				if _, err := tx.Exec(ctx, `
					INSERT INTO stop_etas (trip_id, stop_id, seq, planned_eta, current_eta)
					VALUES ($1,$2,$3,$4,$4) ON CONFLICT (trip_id, stop_id) DO NOTHING`,
					tripID, r.stopID, r.seq, eta); err != nil {
					return err
				}
			}
			_, err = s.emitEvent(ctx, tx, tripID, EvTripGenerated, eventOpts{
				actor:  "system",
				detail: map[string]any{"schedule_id": sc.id, "service_date": date, "planned_departure": sc.departure},
			})
			return err
		}); err != nil {
			return nil, err
		}
	}

	trips, err := s.ListTrips(ctx, &pb.ListTripsRequest{Date: date})
	if err != nil {
		return nil, err
	}
	return &pb.GenerateTripsResponse{Trips: trips.Trips}, nil
}

func (s *Service) ListTrips(ctx context.Context, req *pb.ListTripsRequest) (*pb.ListTripsResponse, error) {
	date := req.Date
	if date == "" {
		date = s.today()
	}
	sql := tripSelect + ` WHERE t.service_date=$1`
	args := []any{date}
	if req.RouteId != 0 {
		sql += ` AND t.route_id=$2`
		args = append(args, req.RouteId)
	}
	sql += ` ORDER BY t.id`
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &pb.ListTripsResponse{}
	for rows.Next() {
		t, err := scanTrip(rows)
		if err != nil {
			return nil, err
		}
		out.Trips = append(out.Trips, t)
	}
	return out, rows.Err()
}

func (s *Service) GetTrip(ctx context.Context, req *pb.GetTripRequest) (*pb.TripDetail, error) {
	return s.getTripDetail(ctx, s.db, req.TripId)
}

func (s *Service) getTripDetail(ctx context.Context, q Querier, tripID int64) (*pb.TripDetail, error) {
	t, err := s.getTrip(ctx, q, tripID)
	if err != nil {
		return nil, err
	}
	students, err := s.loadTripStudents(ctx, q, tripID)
	if err != nil {
		return nil, err
	}
	etas, err := s.loadStopEtas(ctx, q, tripID)
	if err != nil {
		return nil, err
	}
	exs, err := s.loadOpenExceptions(ctx, q, tripID)
	if err != nil {
		return nil, err
	}
	return &pb.TripDetail{Trip: t, Students: students, Etas: etas, OpenExceptions: exs}, nil
}

// ---------------------------------------------------------------------------
// 发车前准备：司机确认车辆、随车老师确认名单
// ---------------------------------------------------------------------------

func (s *Service) ConfirmVehicle(ctx context.Context, req *pb.ConfirmVehicleRequest) (*pb.Trip, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	// 接送角色绑定：仅排班绑定的司机可确认本车
	if req.DriverId != t.DriverId {
		return nil, errPermission(fmt.Sprintf("司机 %d 非本任务排班绑定司机（绑定司机 %d），无权确认车辆", req.DriverId, t.DriverId))
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_SCHEDULED && t.Status != pb.TripStatus_TRIP_STATUS_PREPARING {
		return nil, errPrecond("当前状态不允许确认车辆")
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if req.FuelLiters > 0 {
			if _, err := tx.Exec(ctx, `UPDATE vehicles SET fuel_liters=$1 WHERE id=$2`, req.FuelLiters, t.VehicleId); err != nil {
				return err
			}
		}
		newStatus := "preparing"
		if t.RosterConfirmedAt != nil {
			newStatus = "ready"
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trips SET vehicle_confirmed_at=now(), vehicle_confirm_note=$2, status=$3 WHERE id=$1`,
			req.TripId, req.Note, newStatus); err != nil {
			return err
		}
		_, err := s.emitEvent(ctx, tx, req.TripId, EvVehicleConfirmed, eventOpts{
			actor: fmt.Sprintf("driver:%d", req.DriverId),
			detail: map[string]any{
				"driver_id": req.DriverId, "fuel_liters": req.FuelLiters, "note": req.Note,
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getTrip(ctx, s.db, req.TripId)
}

func (s *Service) ConfirmRoster(ctx context.Context, req *pb.ConfirmRosterRequest) (*pb.Trip, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	// 接送角色绑定：仅排班绑定的随车老师可确认名单
	if req.EscortId != t.EscortId {
		return nil, errPermission(fmt.Sprintf("随车老师 %d 非本任务排班绑定随车老师（绑定随车老师 %d），无权确认名单", req.EscortId, t.EscortId))
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_SCHEDULED && t.Status != pb.TripStatus_TRIP_STATUS_PREPARING {
		return nil, errPrecond("当前状态不允许确认名单")
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		newStatus := "scheduled"
		if t.VehicleConfirmedAt != nil {
			newStatus = "ready"
		}
		if _, err := tx.Exec(ctx, `UPDATE trips SET roster_confirmed_at=now(), status=$2 WHERE id=$1`,
			req.TripId, newStatus); err != nil {
			return err
		}
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM trip_students WHERE trip_id=$1`, req.TripId).Scan(&total); err != nil {
			return err
		}
		_, err := s.emitEvent(ctx, tx, req.TripId, EvRosterConfirmed, eventOpts{
			actor:  fmt.Sprintf("escort:%d", req.EscortId),
			detail: map[string]any{"escort_id": req.EscortId, "student_count": total, "note": req.Note},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getTrip(ctx, s.db, req.TripId)
}

func (s *Service) StartTrip(ctx context.Context, req *pb.StartTripRequest) (*pb.Trip, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.VehicleConfirmedAt == nil || t.RosterConfirmedAt == nil {
		return nil, errPrecond("发车前须先完成司机车辆确认与随车老师名单确认")
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_READY {
		return nil, errPrecond("任务未处于可发车状态")
	}
	now := s.now()
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE trips SET status='in_progress', actual_departure=$2,
			       current_seq=(SELECT min(seq) FROM stop_etas WHERE trip_id=$1)
			WHERE id=$1`, req.TripId, now); err != nil {
			return err
		}
		// 以实际发车时间重算各站 ETA
		if _, err := tx.Exec(ctx, `
			UPDATE stop_etas e SET current_eta=$2::timestamptz + (rs.offset_min || ' minutes')::interval
			FROM route_stops rs
			WHERE rs.stop_id = e.stop_id
			  AND rs.route_id = (SELECT route_id FROM trips WHERE id=$1)
			  AND e.trip_id=$1`, req.TripId, now); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvTripStarted, eventOpts{
			actor:  fmt.Sprintf("driver:%d", t.DriverId),
			detail: map[string]any{"actual_departure": now.Format(time.RFC3339)},
		}); err != nil {
			return err
		}
		// 通知所有应乘学生家长：车辆已发车
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ts.student_id, st.name FROM trip_students ts
			JOIN students st ON st.id=ts.student_id
			WHERE ts.trip_id=$1 AND ts.status IN ('expected')`, req.TripId)
		if err != nil {
			return err
		}
		type stu struct {
			id   int64
			name string
		}
		var stus []stu
		for rows.Next() {
			var x stu
			if err := rows.Scan(&x.id, &x.name); err != nil {
				rows.Close()
				return err
			}
			stus = append(stus, x)
		}
		rows.Close()
		for _, x := range stus {
			if _, err := s.notifyStudent(ctx, tx, req.TripId, x.id, "trip_started",
				fmt.Sprintf("【校车】%s 乘坐的%s（%s）已发车，请留意到站时间。", x.name, t.RouteName, t.VehiclePlate)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.getTrip(ctx, s.db, req.TripId)
}

func (s *Service) ArriveStop(ctx context.Context, req *pb.ArriveStopRequest) (*pb.TripDetail, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("任务不在在途状态")
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE stop_etas SET status='arrived', arrived_at=now() WHERE trip_id=$1 AND stop_id=$2 AND status='pending'`,
			req.TripId, req.StopId)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errInvalid("站点不存在或已到达/跳过")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trips SET current_seq=(SELECT seq FROM stop_etas WHERE trip_id=$1 AND stop_id=$2) WHERE id=$1`,
			req.TripId, req.StopId); err != nil {
			return err
		}
		_, err = s.emitEvent(ctx, tx, req.TripId, EvStopArrived, eventOpts{
			stopID: &req.StopId,
			actor:  fmt.Sprintf("driver:%d", t.DriverId),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getTripDetail(ctx, s.db, req.TripId)
}

// ---------------------------------------------------------------------------
// 上车核验：学生、站点、临时请假、代送授权、坐错车
// ---------------------------------------------------------------------------

// isAuthorizedPerson 核验接送人是否为监护人或当日有效的被授权人（代送/临时换人）。
func (s *Service) isAuthorizedPerson(ctx context.Context, q Querier, studentID int64, name, date string) (bool, error) {
	var n int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM guardians WHERE student_id=$1 AND name=$2`,
		studentID, name).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM pickup_authorizations
		WHERE student_id=$1 AND authorized_name=$2
		  AND (auth_type='regular' OR valid_date=$3)`,
		studentID, name, date).Scan(&n)
	return n > 0, err
}

func (s *Service) getTripStudent(ctx context.Context, q Querier, tripID, studentID int64) (*pb.TripStudent, error) {
	var x pb.TripStudent
	var st string
	var boarded, alighted *time.Time
	err := q.QueryRow(ctx, `
		SELECT ts.id, ts.trip_id, ts.student_id, st.name, c.name, ts.stop_id, sp.name,
		       ts.status, ts.boarded_at, ts.alighted_at, ts.note, st.family_id
		FROM trip_students ts
		JOIN students st ON st.id = ts.student_id
		JOIN classes c   ON c.id = st.class_id
		JOIN stops sp    ON sp.id = ts.stop_id
		WHERE ts.trip_id=$1 AND ts.student_id=$2`, tripID, studentID).
		Scan(&x.Id, &x.TripId, &x.StudentId, &x.StudentName, &x.ClassName, &x.StopId, &x.StopName,
			&st, &boarded, &alighted, &x.Note, &x.FamilyId)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound(fmt.Sprintf("学生 %d 不在任务 %d 名单中", studentID, tripID))
	}
	if err != nil {
		return nil, err
	}
	x.Status = tsStatusFromDB(st)
	x.BoardedAt = tsPtr(boarded)
	x.AlightedAt = tsPtr(alighted)
	return &x, nil
}

func (s *Service) BoardStudent(ctx context.Context, req *pb.BoardRequest) (*pb.BoardResponse, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_READY && t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("任务未处于可上车状态（需先确认车辆与名单）")
	}
	resp := &pb.BoardResponse{}

	ts, err := s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, err
		}
		// 坐错车：学生不在本车名单，查其今日应乘任务
		return s.handleWrongBus(ctx, t, req)
	}

	switch ts.Status {
	case pb.TripStudentStatus_TS_ON_LEAVE:
		return nil, errPrecond("核验失败：该学生今日已请假，不能上车")
	case pb.TripStudentStatus_TS_SELF_PICKUP:
		return nil, errPrecond("核验失败：家长已改为自接，不能上车")
	case pb.TripStudentStatus_TS_CLUB_ACTIVITY:
		return nil, errPrecond("核验失败：学生今日参加社团活动，不乘车")
	case pb.TripStudentStatus_TS_BOARDED:
		return nil, errPrecond("该学生已上车，请勿重复核验")
	case pb.TripStudentStatus_TS_ALIGHTED:
		return nil, errPrecond("该学生已下车")
	}

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		// 缺乘后迟到上车：自动关闭等待记录
		if ts.Status == pb.TripStudentStatus_TS_ABSENT {
			resp.Warnings = append(resp.Warnings, "该学生此前被标记缺乘，本次为迟到上车")
			if _, err := tx.Exec(ctx, `
				UPDATE wait_records SET resolved=true, resolution='学生迟到后上车'
				WHERE trip_id=$1 AND student_id=$2 AND resolved=false`, req.TripId, req.StudentId); err != nil {
				return err
			}
		}
		// 站点核验
		boardStop := ts.StopId
		if req.StopId != 0 && req.StopId != ts.StopId {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("上车站点与分配站点不一致（分配：%s）", ts.StopName))
			boardStop = req.StopId
		}
		// 代送授权核验
		authorized := true
		if req.DroppedOffBy != "" {
			var err error
			authorized, err = s.isAuthorizedPerson(ctx, tx, req.StudentId, req.DroppedOffBy, t.ServiceDate)
			if err != nil {
				return err
			}
			if !authorized {
				resp.Warnings = append(resp.Warnings, fmt.Sprintf("送站人「%s」不在授权名单，已上报学校复核", req.DroppedOffBy))
				if _, err := s.insertException(ctx, tx, req.TripId, pb.ExceptionType_EX_OTHER,
					fmt.Sprintf("学生 %s 的送站人「%s」不在授权名单，随车老师现场核验后放行", ts.StudentName, req.DroppedOffBy),
					req.StudentId, req.Actor); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='boarded', boarded_at=now(), board_stop_id=$3
			WHERE trip_id=$1 AND student_id=$2`, req.TripId, req.StudentId, boardStop); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvStudentBoarded, eventOpts{
			studentID: &req.StudentId,
			stopID:    &boardStop,
			actor:     req.Actor,
			detail: map[string]any{
				"dropped_off_by": req.DroppedOffBy, "dropped_off_authorized": authorized,
				"late_board": ts.Status == pb.TripStudentStatus_TS_ABSENT,
			},
		}); err != nil {
			return err
		}
		_, err := s.notifyStudent(ctx, tx, req.TripId, req.StudentId, "boarded",
			fmt.Sprintf("【校车】%s 已在「%s」上车（%s）。", ts.StudentName, s.stopName(ctx, tx, boardStop), t.VehiclePlate))
		return err
	})
	if err != nil {
		return nil, err
	}
	resp.Student, err = s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
	return resp, err
}

// handleWrongBus 处理学生坐错车：登记异常、通知家长与班主任，可按需强制上车。
func (s *Service) handleWrongBus(ctx context.Context, t *pb.Trip, req *pb.BoardRequest) (*pb.BoardResponse, error) {
	resp := &pb.BoardResponse{WrongBus: true}
	// 查学生今日应乘任务
	var rightTripID int64
	var rightRoute string
	err := s.db.QueryRow(ctx, `
		SELECT t.id, r.name FROM trip_students ts
		JOIN trips t ON t.id=ts.trip_id JOIN routes r ON r.id=t.route_id
		WHERE ts.student_id=$1 AND t.service_date=$2 AND t.id<>$3
		ORDER BY t.id LIMIT 1`, req.StudentId, t.ServiceDate, t.Id).Scan(&rightTripID, &rightRoute)
	notFound := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !notFound {
		return nil, err
	}

	var stuName string
	_ = s.db.QueryRow(ctx, `SELECT name FROM students WHERE id=$1`, req.StudentId).Scan(&stuName)

	detail := fmt.Sprintf("学生 %s 误上 %s（%s）", stuName, t.RouteName, t.VehiclePlate)
	if !notFound {
		detail += fmt.Sprintf("，应乘任务 #%d（%s）", rightTripID, rightRoute)
	}
	resp.Warnings = append(resp.Warnings, detail)

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		ex, err := s.insertException(ctx, tx, t.Id, pb.ExceptionType_EX_WRONG_BUS, detail, req.StudentId, req.Actor)
		if err != nil {
			return err
		}
		resp.Exception = ex
		if _, err := s.emitEvent(ctx, tx, t.Id, EvWrongBus, eventOpts{
			studentID: &req.StudentId,
			actor:     req.Actor,
			detail:    map[string]any{"right_trip_id": rightTripID, "right_route": rightRoute},
		}); err != nil {
			return err
		}
		if _, err := s.notifyStudent(ctx, tx, t.Id, req.StudentId, "wrong_bus",
			fmt.Sprintf("【校车】%s 可能坐错了车（误上 %s），学校正在处理，请保持电话畅通。", stuName, t.VehiclePlate)); err != nil {
			return err
		}
		if err := s.notifyHomeroomOfStudent(ctx, tx, t.Id, req.StudentId, "wrong_bus", "【校车】"+detail); err != nil {
			return err
		}
		if req.Force {
			// 强制登记上车：状态 wrong_bus，等待换乘处置
			if _, err := tx.Exec(ctx, `
				INSERT INTO trip_students (trip_id, student_id, stop_id, status, boarded_at, note)
				VALUES ($1,$2,$3,'wrong_bus',now(),'坐错车登记，待换乘处置')
				ON CONFLICT (trip_id, student_id) DO UPDATE SET status='wrong_bus', boarded_at=now()`,
				t.Id, req.StudentId, req.StopId); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if req.Force {
		resp.Student, _ = s.getTripStudent(ctx, s.db, t.Id, req.StudentId)
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// 下车核验：接站人授权、到站无人接
// ---------------------------------------------------------------------------

func (s *Service) AlightStudent(ctx context.Context, req *pb.AlightRequest) (*pb.AlightResponse, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("任务不在在途状态")
	}
	ts, err := s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
	if err != nil {
		return nil, err
	}
	resp := &pb.AlightResponse{}

	if ts.Status != pb.TripStudentStatus_TS_BOARDED && ts.Status != pb.TripStudentStatus_TS_NO_RECEIVER &&
		ts.Status != pb.TripStudentStatus_TS_WRONG_BUS {
		return nil, errPrecond(fmt.Sprintf("学生当前状态（%s）不允许下车", ts.Status.String()))
	}

	// 到站无人接
	if req.ReceiverName == "" {
		err = s.withTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE trip_students SET status='no_receiver' WHERE trip_id=$1 AND student_id=$2`,
				req.TripId, req.StudentId); err != nil {
				return err
			}
			ex, err := s.insertException(ctx, tx, req.TripId, pb.ExceptionType_EX_NO_RECEIVER,
				fmt.Sprintf("学生 %s 到站「%s」无人接，随车滞留，已联系家长与班主任", ts.StudentName, ts.StopName),
				req.StudentId, req.Actor)
			if err != nil {
				return err
			}
			resp.Exception = ex
			if _, err := s.emitEvent(ctx, tx, req.TripId, EvNoReceiver, eventOpts{
				studentID: &req.StudentId,
				stopID:    &ts.StopId,
				actor:     req.Actor,
			}); err != nil {
				return err
			}
			if _, err := s.notifyStudent(ctx, tx, req.TripId, req.StudentId, "no_receiver",
				fmt.Sprintf("【校车】%s 到站「%s」无人接，孩子暂随车，请立即联系随车老师或到学校接回。", ts.StudentName, ts.StopName)); err != nil {
				return err
			}
			return s.notifyHomeroomOfStudent(ctx, tx, req.TripId, req.StudentId, "no_receiver",
				fmt.Sprintf("【校车】%s 到站无人接，请班主任协助联系家长。", ts.StudentName))
		})
		if err != nil {
			return nil, err
		}
		resp.Warnings = append(resp.Warnings, "到站无人接，学生随车滞留，已上报")
		resp.Student, err = s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
		return resp, err
	}

	// 接站人授权核验
	authorized, err := s.isAuthorizedPerson(ctx, s.db, req.StudentId, req.ReceiverName, t.ServiceDate)
	if err != nil {
		return nil, err
	}
	resp.ReceiverAuthorized = authorized
	if !authorized && !req.ConfirmUnauthorized {
		resp.Warnings = append(resp.Warnings,
			fmt.Sprintf("接站人「%s」不在授权名单，如需放行请随车老师确认（confirm_unauthorized）", req.ReceiverName))
		resp.Student = ts
		return resp, nil
	}

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		alightStop := ts.StopId
		if req.StopId != 0 {
			alightStop = req.StopId
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='alighted', alighted_at=now(), alight_stop_id=$3
			WHERE trip_id=$1 AND student_id=$2`, req.TripId, req.StudentId, alightStop); err != nil {
			return err
		}
		if !authorized {
			if _, err := s.insertException(ctx, tx, req.TripId, pb.ExceptionType_EX_OTHER,
				fmt.Sprintf("未授权接站人「%s」经随车老师现场确认后接走 %s，需学校复核", req.ReceiverName, ts.StudentName),
				req.StudentId, req.Actor); err != nil {
				return err
			}
			resp.Warnings = append(resp.Warnings, "接站人不在授权名单，已按随车老师确认放行并上报学校复核")
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvStudentAlighted, eventOpts{
			studentID: &req.StudentId,
			stopID:    &alightStop,
			actor:     req.Actor,
			detail:    map[string]any{"receiver_name": req.ReceiverName, "receiver_authorized": authorized},
		}); err != nil {
			return err
		}
		_, err := s.notifyStudent(ctx, tx, req.TripId, req.StudentId, "alighted",
			fmt.Sprintf("【校车】%s 已在「%s」下车，由 %s 接走。", ts.StudentName, ts.StopName, req.ReceiverName))
		return err
	})
	if err != nil {
		return nil, err
	}
	resp.Student, err = s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
	return resp, err
}

// ---------------------------------------------------------------------------
// 缺乘处置：等待时间、家长联系次数、是否允许跳站、班主任介入
// ---------------------------------------------------------------------------

func scanWaitRecord(row pgx.Row) (*pb.WaitRecord, error) {
	var w pb.WaitRecord
	var startedAt time.Time
	err := row.Scan(&w.Id, &w.TripId, &w.StudentId, &w.StudentName, &w.StopId, &w.StopName,
		&startedAt, &w.WaitSeconds, &w.ContactedCount, &w.SkipAllowed, &w.TeacherInvolved,
		&w.Resolved, &w.Resolution)
	if err != nil {
		return nil, err
	}
	w.WaitStartedAt = tsVal(startedAt)
	return &w, nil
}

const waitRecordSelect = `
	SELECT w.id, w.trip_id, w.student_id, st.name, w.stop_id, sp.name,
	       w.wait_started_at, w.wait_seconds, w.contacted_count, w.skip_allowed,
	       w.teacher_involved, w.resolved, w.resolution
	FROM wait_records w
	JOIN students st ON st.id=w.student_id
	JOIN stops sp ON sp.id=w.stop_id`

func (s *Service) MarkAbsent(ctx context.Context, req *pb.MarkAbsentRequest) (*pb.WaitRecord, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("任务不在在途状态")
	}
	ts, err := s.getTripStudent(ctx, s.db, req.TripId, req.StudentId)
	if err != nil {
		return nil, err
	}
	if ts.Status == pb.TripStudentStatus_TS_ABSENT {
		// 已登记缺乘，返回未关闭的等待记录
		return scanWaitRecord(s.db.QueryRow(ctx, waitRecordSelect+`
			WHERE w.trip_id=$1 AND w.student_id=$2 AND w.resolved=false ORDER BY w.id DESC LIMIT 1`,
			req.TripId, req.StudentId))
	}
	if ts.Status != pb.TripStudentStatus_TS_EXPECTED {
		return nil, errPrecond(fmt.Sprintf("学生当前状态（%s）不能标记缺乘", ts.Status.String()))
	}
	stopID := req.StopId
	if stopID == 0 {
		stopID = ts.StopId
	}
	var waitID int64
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE trip_students SET status='absent' WHERE trip_id=$1 AND student_id=$2`,
			req.TripId, req.StudentId); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO wait_records (trip_id, student_id, stop_id) VALUES ($1,$2,$3) RETURNING id`,
			req.TripId, req.StudentId, stopID).Scan(&waitID); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvStudentAbsent, eventOpts{
			studentID: &req.StudentId,
			stopID:    &stopID,
			actor:     req.Actor,
		}); err != nil {
			return err
		}
		_, err := s.notifyStudent(ctx, tx, req.TripId, req.StudentId, "absent",
			fmt.Sprintf("【校车】%s 未在站点「%s」出现，随车老师将电话联系您，如需请假请回复。", ts.StudentName, ts.StopName))
		return err
	})
	if err != nil {
		return nil, err
	}
	return scanWaitRecord(s.db.QueryRow(ctx, waitRecordSelect+` WHERE w.id=$1`, waitID))
}

func (s *Service) RecordContact(ctx context.Context, req *pb.RecordContactRequest) (*pb.WaitRecord, error) {
	var waitID int64
	err := s.db.QueryRow(ctx, `
		SELECT id FROM wait_records WHERE trip_id=$1 AND student_id=$2 AND resolved=false
		ORDER BY id DESC LIMIT 1`, req.TripId, req.StudentId).Scan(&waitID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("该学生没有未关闭的缺乘等待记录")
	}
	if err != nil {
		return nil, err
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO contact_logs (trip_id, student_id, wait_record_id, phone, result, actor)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			req.TripId, req.StudentId, waitID, req.Phone, req.Result, req.Actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE wait_records SET contacted_count=contacted_count+1 WHERE id=$1`, waitID); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT contacted_count FROM wait_records WHERE id=$1`, waitID).Scan(&count); err != nil {
			return err
		}
		_, err := s.emitEvent(ctx, tx, req.TripId, EvParentContacted, eventOpts{
			studentID: &req.StudentId,
			actor:     req.Actor,
			detail:    map[string]any{"phone": req.Phone, "result": req.Result, "contacted_count": count},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return scanWaitRecord(s.db.QueryRow(ctx, waitRecordSelect+` WHERE w.id=$1`, waitID))
}

func (s *Service) UpdateWaitRecord(ctx context.Context, req *pb.UpdateWaitRecordRequest) (*pb.WaitRecord, error) {
	w, err := scanWaitRecord(s.db.QueryRow(ctx, waitRecordSelect+` WHERE w.id=$1`, req.WaitRecordId))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("等待记录不存在")
		}
		return nil, err
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE wait_records SET wait_seconds=$2, skip_allowed=$3, teacher_involved=$4, resolved=$5, resolution=$6
			WHERE id=$1`,
			req.WaitRecordId, req.WaitSeconds, req.SkipAllowed, req.TeacherInvolved, req.Resolved, req.Resolution); err != nil {
			return err
		}
		// 允许跳站：学生状态改为 skipped
		if req.SkipAllowed && !w.SkipAllowed {
			if _, err := tx.Exec(ctx, `
				UPDATE trip_students SET status='skipped', note='家长同意跳站，不再等待'
				WHERE trip_id=$1 AND student_id=$2 AND status='absent'`, w.TripId, w.StudentId); err != nil {
				return err
			}
			if _, err := s.emitEvent(ctx, tx, w.TripId, EvSkipStopAuthorized, eventOpts{
				studentID: &w.StudentId,
				stopID:    &w.StopId,
				detail:    map[string]any{"wait_seconds": req.WaitSeconds},
			}); err != nil {
				return err
			}
		}
		// 班主任介入：通知班主任
		if req.TeacherInvolved && !w.TeacherInvolved {
			if err := s.notifyHomeroomOfStudent(ctx, tx, w.TripId, w.StudentId, "teacher_involved",
				fmt.Sprintf("【校车】%s 缺乘且联系家长困难，需要班主任介入跟进。", w.StudentName)); err != nil {
				return err
			}
		}
		_, err := s.emitEvent(ctx, tx, w.TripId, EvWaitUpdated, eventOpts{
			studentID: &w.StudentId,
			stopID:    &w.StopId,
			detail: map[string]any{
				"wait_seconds": req.WaitSeconds, "skip_allowed": req.SkipAllowed,
				"teacher_involved": req.TeacherInvolved, "resolved": req.Resolved, "resolution": req.Resolution,
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return scanWaitRecord(s.db.QueryRow(ctx, waitRecordSelect+` WHERE w.id=$1`, req.WaitRecordId))
}

// ---------------------------------------------------------------------------
// 完成与班主任确认
// ---------------------------------------------------------------------------

func (s *Service) CompleteTrip(ctx context.Context, req *pb.CompleteTripRequest) (*pb.Trip, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("任务不在在途状态")
	}
	// 未闭环的学生状态阻断完成
	rows, err := s.db.Query(ctx, `
		SELECT st.name, ts.status FROM trip_students ts JOIN students st ON st.id=ts.student_id
		WHERE ts.trip_id=$1 AND ts.status IN ('expected','boarded','no_receiver','wrong_bus')`, req.TripId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blocking []string
	for rows.Next() {
		var name, st string
		if err := rows.Scan(&name, &st); err != nil {
			return nil, err
		}
		blocking = append(blocking, name+"("+st+")")
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(blocking) > 0 {
		return nil, errPrecond("以下学生状态未闭环，不能完成任务: "+strings.Join(blocking, "、"))
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE trips SET status='completed', actual_arrival=now() WHERE id=$1`, req.TripId); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvTripCompleted, eventOpts{
			actor: fmt.Sprintf("driver:%d", t.DriverId),
		}); err != nil {
			return err
		}
		// 通知相关班主任确认学生状态
		clsRows, err := tx.Query(ctx, `
			SELECT DISTINCT c.homeroom_teacher, c.homeroom_phone FROM trip_students ts
			JOIN students st ON st.id=ts.student_id JOIN classes c ON c.id=st.class_id
			WHERE ts.trip_id=$1 AND c.homeroom_phone<>''`, req.TripId)
		if err != nil {
			return err
		}
		type teacher struct{ name, phone string }
		var teachers []teacher
		for clsRows.Next() {
			var tc teacher
			if err := clsRows.Scan(&tc.name, &tc.phone); err != nil {
				clsRows.Close()
				return err
			}
			teachers = append(teachers, tc)
		}
		clsRows.Close()
		for _, tc := range teachers {
			if _, err := s.insertNotification(ctx, tx, req.TripId, 0, tc.phone, "confirm_request",
				fmt.Sprintf("【校车】%s（%s）已完成接送，请班主任 %s 确认本班学生到校/到家状态。", t.RouteName, t.VehiclePlate, tc.name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.getTrip(ctx, s.db, req.TripId)
}

func (s *Service) HomeroomConfirm(ctx context.Context, req *pb.HomeroomConfirmRequest) (*pb.Trip, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_COMPLETED && t.Status != pb.TripStatus_TRIP_STATUS_CLOSED {
		return nil, errPrecond("任务完成后班主任才能确认学生状态")
	}
	st := hmStatusToDB(req.Status)
	if st == "" {
		return nil, errInvalid("确认状态无效")
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO homeroom_confirmations (trip_id, student_id, teacher_name, status, note)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (trip_id, student_id) DO UPDATE
			  SET teacher_name=EXCLUDED.teacher_name, status=EXCLUDED.status, note=EXCLUDED.note, confirmed_at=now()`,
			req.TripId, req.StudentId, req.TeacherName, st, req.Note); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvHomeroomConfirmed, eventOpts{
			studentID: &req.StudentId,
			actor:     req.TeacherName,
			detail:    map[string]any{"status": st, "note": req.Note},
		}); err != nil {
			return err
		}
		// 全部学生确认完毕 -> 任务闭环
		var remaining int64
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM trip_students ts
			LEFT JOIN homeroom_confirmations hc ON hc.trip_id=ts.trip_id AND hc.student_id=ts.student_id
			WHERE ts.trip_id=$1 AND hc.id IS NULL`, req.TripId).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			if _, err := tx.Exec(ctx, `UPDATE trips SET status='closed' WHERE id=$1`, req.TripId); err != nil {
				return err
			}
			if _, err := s.emitEvent(ctx, tx, req.TripId, EvTripClosed, eventOpts{
				actor: req.TeacherName,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.getTrip(ctx, s.db, req.TripId)
}

// ---------------------------------------------------------------------------
// 轨迹 / 异常 / 离线补录
// ---------------------------------------------------------------------------

func (s *Service) GetTripTrace(ctx context.Context, req *pb.GetTripTraceRequest) (*pb.GetTripTraceResponse, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
		SELECT e.id, e.trip_id, e.event_type, COALESCE(e.student_id,0), COALESCE(st.name,''),
		       COALESCE(e.stop_id,0), COALESCE(sp.name,''), e.actor, e.detail, e.offline, e.occurred_at, e.synced_at
		FROM trip_events e
		LEFT JOIN students st ON st.id=e.student_id
		LEFT JOIN stops sp ON sp.id=e.stop_id
		WHERE e.trip_id=$1 ORDER BY e.id`, req.TripId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resp := &pb.GetTripTraceResponse{Trip: t}
	for rows.Next() {
		var e pb.TripEvent
		var detailRaw []byte
		var occurredAt time.Time
		var syncedAt *time.Time
		if err := rows.Scan(&e.Id, &e.TripId, &e.EventType, &e.StudentId, &e.StudentName,
			&e.StopId, &e.StopName, &e.Actor, &detailRaw, &e.Offline, &occurredAt, &syncedAt); err != nil {
			return nil, err
		}
		var m map[string]any
		if len(detailRaw) > 0 {
			_ = json.Unmarshal(detailRaw, &m)
		}
		e.Detail = structFromMap(m)
		e.OccurredAt = tsVal(occurredAt)
		e.SyncedAt = tsPtr(syncedAt)
		resp.Events = append(resp.Events, &e)
	}
	return resp, rows.Err()
}

// insertException 落库一条异常并返回完整记录。
func (s *Service) insertException(ctx context.Context, q Querier, tripID int64, etype pb.ExceptionType, detail string, studentID int64, reportedBy string) (*pb.Exception, error) {
	var sid *int64
	if studentID != 0 {
		sid = &studentID
	}
	var id int64
	var createdAt time.Time
	err := q.QueryRow(ctx, `
		INSERT INTO exceptions (trip_id, etype, detail, student_id, reported_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		tripID, exTypeToDB(etype), detail, sid, reportedBy).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}
	return &pb.Exception{
		Id: id, TripId: tripID, Type: etype, Detail: detail, StudentId: studentID,
		ReportedBy: reportedBy, Status: pb.ExceptionStatus_EX_STATUS_OPEN, CreatedAt: tsVal(createdAt),
	}, nil
}

func (s *Service) ReportException(ctx context.Context, req *pb.ReportExceptionRequest) (*pb.Exception, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if req.Type == pb.ExceptionType_EXCEPTION_TYPE_UNSPECIFIED {
		return nil, errInvalid("异常类型不能为空")
	}
	var ex *pb.Exception
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		ex, err = s.insertException(ctx, tx, req.TripId, req.Type, req.Detail, req.StudentId, req.ReportedBy)
		if err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvExceptionReported, eventOpts{
			studentID: &req.StudentId,
			actor:     req.ReportedBy,
			detail:    map[string]any{"type": exTypeToDB(req.Type), "detail": req.Detail},
		}); err != nil {
			return err
		}
		switch req.Type {
		case pb.ExceptionType_EX_VEHICLE_OFFLINE, pb.ExceptionType_EX_NO_SIGNAL:
			// 车辆离线 / 乡村道路无信号：进入降级模式，事件可离线补录
			if _, err := tx.Exec(ctx, `UPDATE trips SET offline_mode=true WHERE id=$1`, req.TripId); err != nil {
				return err
			}
		case pb.ExceptionType_EX_CLASS_RESCHEDULED:
			// 学校临时调课：未发车的任务改发车时间并重算 ETA、通知家长
			if req.NewDeparture != "" && t.ActualDeparture == nil {
				base, err := s.combineDateTime(t.ServiceDate, req.NewDeparture)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE trips SET planned_departure=$2 WHERE id=$1`,
					req.TripId, req.NewDeparture); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `
					UPDATE stop_etas e SET planned_eta=$2::timestamptz + (rs.offset_min || ' minutes')::interval,
					                       current_eta=$2::timestamptz + (rs.offset_min || ' minutes')::interval
					FROM route_stops rs
					WHERE rs.stop_id = e.stop_id
					  AND rs.route_id = (SELECT route_id FROM trips WHERE id=$1)
					  AND e.trip_id=$1 AND e.status='pending'`,
					req.TripId, base); err != nil {
					return err
				}
				if _, err := s.emitEvent(ctx, tx, req.TripId, EvClassRescheduled, eventOpts{
					actor:  req.ReportedBy,
					detail: map[string]any{"new_departure": req.NewDeparture},
				}); err != nil {
					return err
				}
				rows, err := tx.Query(ctx, `SELECT student_id FROM trip_students WHERE trip_id=$1`, req.TripId)
				if err != nil {
					return err
				}
				var ids []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					ids = append(ids, id)
				}
				rows.Close()
				for _, sid := range ids {
					if _, err := s.notifyStudent(ctx, tx, req.TripId, sid, "class_rescheduled",
						fmt.Sprintf("【校车】学校临时调课，%s 发车时间调整为 %s，请留意新的到站时间。",
							dirLabel(t.Direction), req.NewDeparture)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

func (s *Service) ResolveException(ctx context.Context, req *pb.ResolveExceptionRequest) (*pb.Exception, error) {
	var ex pb.Exception
	var etype, status string
	var createdAt time.Time
	err := s.db.QueryRow(ctx, `
		SELECT id, trip_id, etype, detail, status, COALESCE(student_id,0), reported_by, created_at
		FROM exceptions WHERE id=$1`, req.ExceptionId).
		Scan(&ex.Id, &ex.TripId, &etype, &ex.Detail, &status, &ex.StudentId, &ex.ReportedBy, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("异常记录不存在")
	}
	if err != nil {
		return nil, err
	}
	ex.Type = exTypeFromDB(etype)
	ex.CreatedAt = tsVal(createdAt)

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE exceptions SET status='resolved', resolution=$2, resolved_by=$3, resolved_at=now()
			WHERE id=$1`, req.ExceptionId, req.Resolution, req.ResolvedBy); err != nil {
			return err
		}
		if _, err := s.emitEvent(ctx, tx, ex.TripId, EvExceptionResolved, eventOpts{
			studentID: &ex.StudentId,
			actor:     req.ResolvedBy,
			detail:    map[string]any{"exception_id": req.ExceptionId, "type": etype, "resolution": req.Resolution},
		}); err != nil {
			return err
		}
		// 离线类异常全部关闭后退出降级模式
		if etype == "vehicle_offline" || etype == "no_signal" {
			var open int64
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM exceptions
				WHERE trip_id=$1 AND etype IN ('vehicle_offline','no_signal') AND status='open'`,
				ex.TripId).Scan(&open); err != nil {
				return err
			}
			if open == 0 {
				if _, err := tx.Exec(ctx, `UPDATE trips SET offline_mode=false WHERE id=$1`, ex.TripId); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ex.Status = pb.ExceptionStatus_EX_STATUS_RESOLVED
	ex.Resolution = req.Resolution
	ex.ResolvedBy = req.ResolvedBy
	now := s.now()
	ex.ResolvedAt = tsVal(now)
	return &ex, nil
}

// SyncOfflineEvents 乡村道路无信号恢复后，批量补录离线期间的事件。
func (s *Service) SyncOfflineEvents(ctx context.Context, req *pb.SyncOfflineEventsRequest) (*pb.SyncOfflineEventsResponse, error) {
	if _, err := s.getTrip(ctx, s.db, req.TripId); err != nil {
		return nil, err
	}
	resp := &pb.SyncOfflineEventsResponse{}
	for i, ev := range req.Events {
		occurredAt := s.now()
		if ev.OccurredAt != nil {
			occurredAt = ev.OccurredAt.AsTime()
		}
		var err error
		switch ev.EventType {
		case "board":
			err = s.applyOfflineBoard(ctx, req.TripId, ev.StudentId, ev.StopId, req.Actor, occurredAt, ev.Note)
		case "alight":
			err = s.applyOfflineAlight(ctx, req.TripId, ev.StudentId, ev.StopId, req.Actor, occurredAt, ev.Note)
		case "absent":
			err = s.applyOfflineAbsent(ctx, req.TripId, ev.StudentId, ev.StopId, req.Actor, occurredAt, ev.Note)
		default:
			err = fmt.Errorf("未知事件类型 %q", ev.EventType)
		}
		if err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("事件#%d: %v", i+1, err))
		} else {
			resp.Applied++
		}
	}
	if resp.Applied > 0 {
		if _, err := s.emitEvent(ctx, s.db, req.TripId, EvOfflineSync, eventOpts{
			actor:  req.Actor,
			detail: map[string]any{"applied": resp.Applied, "errors": len(resp.Errors)},
		}); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (s *Service) applyOfflineBoard(ctx context.Context, tripID, studentID, stopID int64, actor string, at time.Time, note string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='boarded', boarded_at=$3, board_stop_id=$4
			WHERE trip_id=$1 AND student_id=$2 AND status IN ('expected','absent')`,
			tripID, studentID, at, stopID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("学生 %d 状态不允许补录上车", studentID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE wait_records SET resolved=true, resolution='离线补录：学生已上车'
			WHERE trip_id=$1 AND student_id=$2 AND resolved=false`, tripID, studentID); err != nil {
			return err
		}
		_, err = s.emitEvent(ctx, tx, tripID, EvStudentBoarded, eventOpts{
			studentID: &studentID, stopID: &stopID, actor: actor,
			detail: map[string]any{"note": note}, offline: true, occurredAt: at, synced: true,
		})
		return err
	})
}

func (s *Service) applyOfflineAlight(ctx context.Context, tripID, studentID, stopID int64, actor string, at time.Time, note string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='alighted', alighted_at=$3, alight_stop_id=$4
			WHERE trip_id=$1 AND student_id=$2 AND status IN ('boarded','no_receiver')`,
			tripID, studentID, at, stopID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("学生 %d 状态不允许补录下车", studentID)
		}
		_, err = s.emitEvent(ctx, tx, tripID, EvStudentAlighted, eventOpts{
			studentID: &studentID, stopID: &stopID, actor: actor,
			detail: map[string]any{"note": note}, offline: true, occurredAt: at, synced: true,
		})
		return err
	})
}

func (s *Service) applyOfflineAbsent(ctx context.Context, tripID, studentID, stopID int64, actor string, at time.Time, note string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE trip_students SET status='absent'
			WHERE trip_id=$1 AND student_id=$2 AND status='expected'`, tripID, studentID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("学生 %d 状态不允许补录缺乘", studentID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wait_records (trip_id, student_id, stop_id, wait_started_at)
			VALUES ($1,$2,$3,$4)`, tripID, studentID, stopID, at); err != nil {
			return err
		}
		_, err = s.emitEvent(ctx, tx, tripID, EvStudentAbsent, eventOpts{
			studentID: &studentID, stopID: &stopID, actor: actor,
			detail: map[string]any{"note": note}, offline: true, occurredAt: at, synced: true,
		})
		return err
	})
}

// ---------------------------------------------------------------------------
// 请假 / 兄弟姐妹同车
// ---------------------------------------------------------------------------

func (s *Service) ReportLeave(ctx context.Context, req *pb.ReportLeaveRequest) (*pb.LeaveRecord, error) {
	date := req.Date
	if date == "" {
		date = s.today()
	}
	rec := &pb.LeaveRecord{StudentId: req.StudentId, LeaveDate: date, Reason: req.Reason, ReportedBy: req.ReportedBy}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO leave_records (student_id, leave_date, reason, reported_by)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (student_id, leave_date) DO UPDATE SET reason=EXCLUDED.reason, reported_by=EXCLUDED.reported_by
			RETURNING id`,
			req.StudentId, date, req.Reason, req.ReportedBy).Scan(&rec.Id); err != nil {
			return err
		}
		// 当日相关任务中的学生标记请假
		rows, err := tx.Query(ctx, `
			SELECT ts.trip_id FROM trip_students ts JOIN trips t ON t.id=ts.trip_id
			WHERE ts.student_id=$1 AND t.service_date=$2 AND ts.status='expected'`, req.StudentId, date)
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
			if _, err := tx.Exec(ctx, `
				UPDATE trip_students SET status='on_leave', note=$3 WHERE trip_id=$1 AND student_id=$2`,
				tripID, req.StudentId, req.Reason); err != nil {
				return err
			}
			if _, err := s.emitEvent(ctx, tx, tripID, EvLeaveReported, eventOpts{
				studentID: &req.StudentId,
				actor:     req.ReportedBy,
				detail:    map[string]any{"reason": req.Reason, "leave_date": date},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Service) RecordSiblingRide(ctx context.Context, req *pb.SiblingRideRequest) (*pb.TripEvent, error) {
	if len(req.StudentIds) < 2 {
		return nil, errInvalid("兄弟姐妹同车至少需要两名学生")
	}
	if _, err := s.getTrip(ctx, s.db, req.TripId); err != nil {
		return nil, err
	}
	// 校验同一家庭且都在本车
	var familyID int64
	var names []string
	for _, sid := range req.StudentIds {
		var fid int64
		var name string
		if err := s.db.QueryRow(ctx, `SELECT family_id, name FROM students WHERE id=$1`, sid).Scan(&fid, &name); err != nil {
			return nil, errNotFound(fmt.Sprintf("学生 %d 不存在", sid))
		}
		if fid == 0 || (familyID != 0 && fid != familyID) {
			return nil, errPrecond(fmt.Sprintf("学生 %s 与其他学生不属于同一家庭", name))
		}
		familyID = fid
		var cnt int64
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM trip_students WHERE trip_id=$1 AND student_id=$2`,
			req.TripId, sid).Scan(&cnt); err != nil {
			return nil, err
		}
		if cnt == 0 {
			return nil, errPrecond(fmt.Sprintf("学生 %s 不在本趟车名单中", name))
		}
		names = append(names, name)
	}
	var eventID int64
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		id, err := s.emitEvent(ctx, tx, req.TripId, EvSiblingRide, eventOpts{
			actor: req.Actor,
			detail: map[string]any{
				"student_ids": req.StudentIds, "student_names": names, "family_id": familyID, "note": req.Note,
			},
		})
		if err != nil {
			return err
		}
		eventID = id
		for _, sid := range req.StudentIds {
			if _, err := s.notifyStudent(ctx, tx, req.TripId, sid, "sibling_ride",
				fmt.Sprintf("【校车】%s 已登记兄弟姐妹同车（%s）。", strings.Join(names, "、"), req.Note)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &pb.TripEvent{
		Id: eventID, TripId: req.TripId, EventType: EvSiblingRide, Actor: req.Actor,
		Detail: structFromMap(map[string]any{"student_names": names, "note": req.Note}),
		OccurredAt: tsVal(s.now()),
	}, nil
}

// stopName 取站点名（通知文案用，失败时返回空串）。
func (s *Service) stopName(ctx context.Context, q Querier, stopID int64) string {
	var name string
	if err := q.QueryRow(ctx, `SELECT name FROM stops WHERE id=$1`, stopID).Scan(&name); err != nil {
		return ""
	}
	return name
}
