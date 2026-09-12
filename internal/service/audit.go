package service

import (
	"context"
	"time"

	pb "schoolbus/gen/schoolbusv1"
)

// ---------------------------------------------------------------------------
// AuditService：教育局线路履约抽查、学校点名
// ---------------------------------------------------------------------------

func (s *Service) GetRouteFulfillment(ctx context.Context, req *pb.RouteFulfillmentRequest) (*pb.RouteFulfillment, error) {
	date := req.Date
	if date == "" {
		date = s.today()
	}
	out := &pb.RouteFulfillment{RouteId: req.RouteId, Date: date}
	if err := s.db.QueryRow(ctx, `SELECT name FROM routes WHERE id=$1`, req.RouteId).Scan(&out.RouteName); err != nil {
		return nil, errNotFound("线路不存在")
	}

	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.direction, t.status, t.planned_departure, t.actual_departure, t.actual_arrival, t.delay_min
		FROM trips t WHERE t.route_id=$1 AND t.service_date=$2 ORDER BY t.id`, req.RouteId, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type tripRow struct {
		id       int64
		dir, st  string
		planned  string
		actualD  *time.Time
		actualA  *time.Time
		delayMin int
	}
	var trips []tripRow
	for rows.Next() {
		var tr tripRow
		if err := rows.Scan(&tr.id, &tr.dir, &tr.st, &tr.planned, &tr.actualD, &tr.actualA, &tr.delayMin); err != nil {
			return nil, err
		}
		trips = append(trips, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, tr := range trips {
		f := &pb.TripFulfillment{
			TripId: tr.id, Direction: dirFromDB(tr.dir), Status: tripStatusFromDB(tr.st),
			PlannedDeparture: tr.planned, ActualDeparture: tsPtr(tr.actualD),
			ActualArrival: tsPtr(tr.actualA), DelayMin: int32(tr.delayMin),
		}
		// 准点判断：实际发车不晚于计划 5 分钟
		if tr.actualD != nil {
			if plannedAt, err := s.combineDateTime(date, tr.planned); err == nil {
				f.OnTimeDeparture = !tr.actualD.After(plannedAt.Add(5 * time.Minute))
			}
		}
		if err := s.db.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE status='arrived'), count(*) FILTER (WHERE status='skipped')
			FROM stop_etas WHERE trip_id=$1`, tr.id).
			Scan(&f.StopsTotal, &f.StopsArrived, &f.StopsSkipped); err != nil {
			return nil, err
		}
		if err := s.db.QueryRow(ctx, `
			SELECT count(*),
			       count(*) FILTER (WHERE status='boarded'),
			       count(*) FILTER (WHERE status='alighted'),
			       count(*) FILTER (WHERE status='absent'),
			       count(*) FILTER (WHERE status='skipped')
			FROM trip_students WHERE trip_id=$1`, tr.id).
			Scan(&f.StudentsExpected, &f.StudentsBoarded, &f.StudentsAlighted, &f.StudentsAbsent, &f.StudentsSkipped); err != nil {
			return nil, err
		}
		// 已上车后又下车的学生计入 boarded 统计
		f.StudentsBoarded += f.StudentsAlighted
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM reroutes WHERE trip_id=$1`, tr.id).Scan(&f.RerouteCount); err != nil {
			return nil, err
		}
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM exceptions WHERE trip_id=$1`, tr.id).Scan(&f.ExceptionCount); err != nil {
			return nil, err
		}
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM contact_logs WHERE trip_id=$1`, tr.id).Scan(&f.ContactCount); err != nil {
			return nil, err
		}
		out.Trips = append(out.Trips, f)
	}
	return out, nil
}

func (s *Service) GetTripRollcall(ctx context.Context, req *pb.TripRollcallRequest) (*pb.TripRollcall, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	out := &pb.TripRollcall{TripId: req.TripId, TripStatus: t.Status}
	students, err := s.loadTripStudents(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	etas, err := s.loadStopEtas(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	etaByStop := map[int64]*pb.StopEta{}
	for _, e := range etas {
		etaByStop[e.StopId] = e
	}
	for _, st := range students {
		entry := &pb.RollcallEntry{
			StudentId: st.StudentId, StudentName: st.StudentName, ClassName: st.ClassName,
			StopName: st.StopName, Status: st.Status,
		}
		if e, ok := etaByStop[st.StopId]; ok {
			entry.Eta = e.CurrentEta
		}
		var hmStatus string
		err := s.db.QueryRow(ctx, `
			SELECT status FROM homeroom_confirmations WHERE trip_id=$1 AND student_id=$2`,
			req.TripId, st.StudentId).Scan(&hmStatus)
		if err == nil {
			entry.HomeroomStatus = hmStatusFromDB(hmStatus)
		}
		gRows, err := s.db.Query(ctx, `SELECT phone FROM guardians WHERE student_id=$1`, st.StudentId)
		if err != nil {
			return nil, err
		}
		for gRows.Next() {
			var phone string
			if err := gRows.Scan(&phone); err != nil {
				gRows.Close()
				return nil, err
			}
			entry.GuardianPhones = append(entry.GuardianPhones, phone)
		}
		gRows.Close()
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}
