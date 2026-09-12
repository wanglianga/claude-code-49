package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	pb "schoolbus/gen/schoolbusv1"
)

// ---------------------------------------------------------------------------
// AdminService：学校基础数据维护
// ---------------------------------------------------------------------------

func (s *Service) Ping(ctx context.Context, _ *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{
		Service:    "schoolbus",
		Version:    "1.0.0",
		ServerTime: tsVal(s.now()),
	}, nil
}

func (s *Service) CreateSchool(ctx context.Context, in *pb.School) (*pb.School, error) {
	if in.Name == "" {
		return nil, errInvalid("学校名称不能为空")
	}
	var createdAt time.Time
	err := s.db.QueryRow(ctx, `INSERT INTO schools (name, address) VALUES ($1,$2) RETURNING id, created_at`,
		in.Name, in.Address).Scan(&in.Id, &createdAt)
	if err != nil {
		return nil, err
	}
	in.CreatedAt = tsVal(createdAt)
	return in, nil
}

func (s *Service) CreateClass(ctx context.Context, in *pb.Class) (*pb.Class, error) {
	if in.SchoolId == 0 || in.Name == "" {
		return nil, errInvalid("school_id 与班级名称不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO classes (school_id, name, grade, homeroom_teacher, homeroom_phone)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.SchoolId, in.Name, in.Grade, in.HomeroomTeacher, in.HomeroomPhone).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateStudent(ctx context.Context, in *pb.Student) (*pb.Student, error) {
	if in.ClassId == 0 || in.Name == "" {
		return nil, errInvalid("class_id 与学生姓名不能为空")
	}
	if in.Status == "" {
		in.Status = "active"
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO students (class_id, name, student_no, gender, family_id, status)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		in.ClassId, in.Name, in.StudentNo, in.Gender, in.FamilyId, in.Status).Scan(&in.Id)
	if err != nil {
		return nil, err
	}
	_ = s.db.QueryRow(ctx, `SELECT name FROM classes WHERE id=$1`, in.ClassId).Scan(&in.ClassName)
	return in, nil
}

func (s *Service) CreateGuardian(ctx context.Context, in *pb.Guardian) (*pb.Guardian, error) {
	if in.StudentId == 0 || in.Name == "" || in.Phone == "" {
		return nil, errInvalid("student_id、监护人姓名、电话不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO guardians (student_id, name, relation, phone, is_primary)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.StudentId, in.Name, in.Relation, in.Phone, in.IsPrimary).Scan(&in.Id)
	return in, err
}

func (s *Service) CreatePickupAuthorization(ctx context.Context, in *pb.PickupAuthorization) (*pb.PickupAuthorization, error) {
	if in.StudentId == 0 || in.AuthorizedName == "" {
		return nil, errInvalid("student_id 与被授权人姓名不能为空")
	}
	if in.AuthType == "" {
		in.AuthType = "regular"
	}
	var validDate *string
	if in.ValidDate != "" {
		validDate = &in.ValidDate
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO pickup_authorizations (student_id, authorized_name, authorized_phone, relation, auth_type, valid_date, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		in.StudentId, in.AuthorizedName, in.AuthorizedPhone, in.Relation, in.AuthType, validDate, in.CreatedBy).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateStop(ctx context.Context, in *pb.Stop) (*pb.Stop, error) {
	if in.SchoolId == 0 || in.Name == "" {
		return nil, errInvalid("school_id 与站点名称不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO stops (school_id, name, address, latitude, longitude)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.SchoolId, in.Name, in.Address, in.Latitude, in.Longitude).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateTimeWindow(ctx context.Context, in *pb.TimeWindow) (*pb.TimeWindow, error) {
	if in.SchoolId == 0 || in.Name == "" {
		return nil, errInvalid("school_id 与时段名称不能为空")
	}
	dir := dirToDB(in.Direction)
	if dir == "" {
		return nil, errInvalid("direction 无效")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO time_windows (school_id, name, direction, start_time, end_time)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.SchoolId, in.Name, dir, in.StartTime, in.EndTime).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateVehicle(ctx context.Context, in *pb.Vehicle) (*pb.Vehicle, error) {
	if in.PlateNo == "" {
		return nil, errInvalid("车牌号不能为空")
	}
	if in.Status == "" {
		in.Status = "available"
	}
	if in.FuelPerKm == 0 {
		in.FuelPerKm = 0.25
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO vehicles (plate_no, capacity, fuel_liters, fuel_per_km, status)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		in.PlateNo, in.Capacity, in.FuelLiters, in.FuelPerKm, in.Status).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateDriver(ctx context.Context, in *pb.Driver) (*pb.Driver, error) {
	if in.Name == "" {
		return nil, errInvalid("司机姓名不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO drivers (name, phone, license_no) VALUES ($1,$2,$3) RETURNING id`,
		in.Name, in.Phone, in.LicenseNo).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateEscort(ctx context.Context, in *pb.Escort) (*pb.Escort, error) {
	if in.Name == "" {
		return nil, errInvalid("随车老师姓名不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO escorts (name, phone, school_id) VALUES ($1,$2,$3) RETURNING id`,
		in.Name, in.Phone, in.SchoolId).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateRoute(ctx context.Context, in *pb.CreateRouteRequest) (*pb.Route, error) {
	if in.SchoolId == 0 || in.Name == "" {
		return nil, errInvalid("school_id 与线路名称不能为空")
	}
	dir := dirToDB(in.Direction)
	if dir == "" {
		return nil, errInvalid("direction 无效")
	}
	if len(in.Stops) == 0 {
		return nil, errInvalid("线路至少包含一个站点")
	}
	route := &pb.Route{SchoolId: in.SchoolId, Name: in.Name, Direction: in.Direction, Active: true}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO routes (school_id, name, direction) VALUES ($1,$2,$3) RETURNING id`,
			in.SchoolId, in.Name, dir).Scan(&route.Id); err != nil {
			return err
		}
		for _, st := range in.Stops {
			if _, err := tx.Exec(ctx, `
				INSERT INTO route_stops (route_id, stop_id, seq, offset_min) VALUES ($1,$2,$3,$4)
				ON CONFLICT (route_id, seq) DO UPDATE SET stop_id=EXCLUDED.stop_id, offset_min=EXCLUDED.offset_min`,
				route.Id, st.StopId, st.Seq, st.OffsetMin); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	route.Stops, err = s.loadRouteStops(ctx, route.Id)
	return route, err
}

func (s *Service) loadRouteStops(ctx context.Context, routeID int64) ([]*pb.RouteStop, error) {
	rows, err := s.db.Query(ctx, `
		SELECT rs.stop_id, sp.name, rs.seq, rs.offset_min
		FROM route_stops rs JOIN stops sp ON sp.id=rs.stop_id
		WHERE rs.route_id=$1 ORDER BY rs.seq`, routeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.RouteStop
	for rows.Next() {
		var rs pb.RouteStop
		if err := rows.Scan(&rs.StopId, &rs.StopName, &rs.Seq, &rs.OffsetMin); err != nil {
			return nil, err
		}
		out = append(out, &rs)
	}
	return out, rows.Err()
}

func (s *Service) AssignStudentRoute(ctx context.Context, in *pb.StudentRouteAssignment) (*pb.StudentRouteAssignment, error) {
	dir := dirToDB(in.Direction)
	if in.StudentId == 0 || in.RouteId == 0 || in.StopId == 0 || dir == "" {
		return nil, errInvalid("student_id、route_id、stop_id、direction 均不能为空")
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO student_routes (student_id, route_id, stop_id, direction) VALUES ($1,$2,$3,$4)
		ON CONFLICT (student_id, route_id, direction) DO UPDATE SET stop_id=EXCLUDED.stop_id
		RETURNING id`,
		in.StudentId, in.RouteId, in.StopId, dir).Scan(&in.Id)
	return in, err
}

func (s *Service) CreateSchedule(ctx context.Context, in *pb.Schedule) (*pb.Schedule, error) {
	dir := dirToDB(in.Direction)
	if in.RouteId == 0 || in.VehicleId == 0 || in.DriverId == 0 || in.EscortId == 0 {
		return nil, errInvalid("route/vehicle/driver/escort 均不能为空")
	}
	if in.ServiceDate == "" || in.PlannedDeparture == "" {
		return nil, errInvalid("service_date 与 planned_departure 不能为空")
	}
	if in.Status == "" {
		in.Status = "planned"
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO schedules (route_id, vehicle_id, driver_id, escort_id, service_date, direction, planned_departure, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (route_id, service_date, direction) DO UPDATE
		  SET vehicle_id=EXCLUDED.vehicle_id, driver_id=EXCLUDED.driver_id,
		      escort_id=EXCLUDED.escort_id, planned_departure=EXCLUDED.planned_departure
		RETURNING id`,
		in.RouteId, in.VehicleId, in.DriverId, in.EscortId, in.ServiceDate, dir, in.PlannedDeparture, in.Status).Scan(&in.Id)
	return in, err
}

func (s *Service) GetOverview(ctx context.Context, _ *pb.GetOverviewRequest) (*pb.Overview, error) {
	o := &pb.Overview{}
	counts := []struct {
		sql string
		dst *int64
	}{
		{`SELECT count(*) FROM schools`, &o.Schools},
		{`SELECT count(*) FROM classes`, &o.Classes},
		{`SELECT count(*) FROM students`, &o.Students},
		{`SELECT count(*) FROM guardians`, &o.Guardians},
		{`SELECT count(*) FROM stops`, &o.Stops},
		{`SELECT count(*) FROM vehicles`, &o.Vehicles},
		{`SELECT count(*) FROM drivers`, &o.Drivers},
		{`SELECT count(*) FROM escorts`, &o.Escorts},
		{`SELECT count(*) FROM routes`, &o.Routes},
		{`SELECT count(*) FROM schedules`, &o.Schedules},
	}
	for _, c := range counts {
		if err := s.db.QueryRow(ctx, c.sql).Scan(c.dst); err != nil {
			return nil, err
		}
	}

	trips, err := s.ListTrips(ctx, &pb.ListTripsRequest{Date: s.today()})
	if err != nil {
		return nil, err
	}
	o.TodayTrips = trips.Trips

	stuRows, err := s.db.Query(ctx, `
		SELECT st.id, st.class_id, c.name, st.name, st.student_no, st.gender, st.family_id, st.status
		FROM students st JOIN classes c ON c.id=st.class_id ORDER BY st.id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer stuRows.Close()
	for stuRows.Next() {
		var st pb.Student
		if err := stuRows.Scan(&st.Id, &st.ClassId, &st.ClassName, &st.Name, &st.StudentNo, &st.Gender, &st.FamilyId, &st.Status); err != nil {
			return nil, err
		}
		o.StudentList = append(o.StudentList, &st)
	}
	if err := stuRows.Err(); err != nil {
		return nil, err
	}

	vehRows, err := s.db.Query(ctx, `SELECT id, plate_no, capacity, fuel_liters, fuel_per_km, status FROM vehicles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer vehRows.Close()
	for vehRows.Next() {
		var v pb.Vehicle
		if err := vehRows.Scan(&v.Id, &v.PlateNo, &v.Capacity, &v.FuelLiters, &v.FuelPerKm, &v.Status); err != nil {
			return nil, err
		}
		o.VehicleList = append(o.VehicleList, &v)
	}
	if err := vehRows.Err(); err != nil {
		return nil, err
	}

	routeRows, err := s.db.Query(ctx, `SELECT id, school_id, name, direction, active FROM routes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer routeRows.Close()
	for routeRows.Next() {
		var r pb.Route
		var dir string
		if err := routeRows.Scan(&r.Id, &r.SchoolId, &r.Name, &dir, &r.Active); err != nil {
			return nil, err
		}
		r.Direction = dirFromDB(dir)
		r.Stops, err = s.loadRouteStops(ctx, r.Id)
		if err != nil {
			return nil, err
		}
		o.RouteList = append(o.RouteList, &r)
	}
	return o, routeRows.Err()
}
