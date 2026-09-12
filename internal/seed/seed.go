// Package seed 在空库时写入演示数据（学校/班级/学生/监护人/授权/站点/时段/
// 车辆/司机/随车老师/线路/排班），并生成当日接送任务。
package seed

import (
	"context"
	"fmt"

	pb "schoolbus/gen/schoolbusv1"
	"schoolbus/internal/service"
)

// Run 幂等：已有学校数据时直接跳过。
func Run(ctx context.Context, svc *service.Service, today string) error {
	var n int64
	if err := svc.DB().QueryRow(ctx, `SELECT count(*) FROM schools`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	school, err := svc.CreateSchool(ctx, &pb.School{Name: "云溪县第一小学", Address: "云溪县城关镇育才路 12 号"})
	if err != nil {
		return err
	}

	class1, err := svc.CreateClass(ctx, &pb.Class{SchoolId: school.Id, Name: "三年级1班", Grade: "三年级", HomeroomTeacher: "王莉", HomeroomPhone: "13800000011"})
	if err != nil {
		return err
	}
	class2, err := svc.CreateClass(ctx, &pb.Class{SchoolId: school.Id, Name: "三年级2班", Grade: "三年级", HomeroomTeacher: "陈强", HomeroomPhone: "13800000012"})
	if err != nil {
		return err
	}

	type stuSeed struct {
		name, no, gender string
		classID          int64
		familyID         int64
	}
	students := []stuSeed{
		{"王小明", "20230101", "男", class1.Id, 1001}, // 与王小红为兄妹
		{"王小红", "20230102", "女", class1.Id, 1001},
		{"李华", "20230103", "男", class1.Id, 0},
		{"张倩", "20230104", "女", class1.Id, 0},
		{"刘洋", "20230201", "男", class2.Id, 0},
		{"陈晨", "20230202", "女", class2.Id, 0},
		{"赵磊", "20230203", "男", class2.Id, 0},
		{"孙丽", "20230105", "女", class1.Id, 0},
	}
	stuIDs := map[string]int64{}
	for _, st := range students {
		s, err := svc.CreateStudent(ctx, &pb.Student{ClassId: st.classID, Name: st.name, StudentNo: st.no, Gender: st.gender, FamilyId: st.familyID})
		if err != nil {
			return err
		}
		stuIDs[st.name] = s.Id
	}

	type gSeed struct {
		stu, name, relation, phone string
		primary                    bool
	}
	guardians := []gSeed{
		{"王小明", "王建国", "父亲", "13900010001", true},
		{"王小明", "李秀英", "母亲", "13900010002", false},
		{"王小红", "王建国", "父亲", "13900010001", true},
		{"王小红", "李秀英", "母亲", "13900010002", false},
		{"李华", "李强", "父亲", "13900020001", true},
		{"张倩", "张敏", "母亲", "13900030001", true},
		{"刘洋", "刘军", "父亲", "13900040001", true},
		{"陈晨", "陈静", "母亲", "13900050001", true},
		{"赵磊", "赵刚", "父亲", "13900060001", true},
		{"孙丽", "孙燕", "母亲", "13900070001", true},
	}
	for _, g := range guardians {
		if _, err := svc.CreateGuardian(ctx, &pb.Guardian{
			StudentId: stuIDs[g.stu], Name: g.name, Relation: g.relation, Phone: g.phone, IsPrimary: g.primary,
		}); err != nil {
			return err
		}
	}

	// 长期代接授权
	if _, err := svc.CreatePickupAuthorization(ctx, &pb.PickupAuthorization{
		StudentId: stuIDs["李华"], AuthorizedName: "李老汉", AuthorizedPhone: "13900020002",
		Relation: "祖父", AuthType: "regular", CreatedBy: "school",
	}); err != nil {
		return err
	}
	if _, err := svc.CreatePickupAuthorization(ctx, &pb.PickupAuthorization{
		StudentId: stuIDs["张倩"], AuthorizedName: "王婆婆", AuthorizedPhone: "13900030002",
		Relation: "外祖母", AuthType: "regular", CreatedBy: "school",
	}); err != nil {
		return err
	}

	stopNames := []string{"县城东站", "河口村委", "石桥镇口", "柳树湾", "学校正门"}
	stopIDs := map[string]int64{}
	for i, name := range stopNames {
		st, err := svc.CreateStop(ctx, &pb.Stop{
			SchoolId: school.Id, Name: name, Address: fmt.Sprintf("云溪县%s站点", name),
			Latitude: 29.4 + float64(i)*0.01, Longitude: 106.5 + float64(i)*0.01,
		})
		if err != nil {
			return err
		}
		stopIDs[name] = st.Id
	}

	if _, err := svc.CreateTimeWindow(ctx, &pb.TimeWindow{SchoolId: school.Id, Name: "早接时段", Direction: pb.Direction_DIRECTION_MORNING_PICKUP, StartTime: "06:30", EndTime: "08:00"}); err != nil {
		return err
	}
	if _, err := svc.CreateTimeWindow(ctx, &pb.TimeWindow{SchoolId: school.Id, Name: "晚送时段", Direction: pb.Direction_DIRECTION_AFTERNOON_DROPOFF, StartTime: "16:30", EndTime: "18:00"}); err != nil {
		return err
	}

	veh1, err := svc.CreateVehicle(ctx, &pb.Vehicle{PlateNo: "云A·D12345", Capacity: 41, FuelLiters: 120, FuelPerKm: 0.28})
	if err != nil {
		return err
	}
	if _, err := svc.CreateVehicle(ctx, &pb.Vehicle{PlateNo: "云A·D67890", Capacity: 35, FuelLiters: 90, FuelPerKm: 0.25}); err != nil {
		return err
	}

	driver1, err := svc.CreateDriver(ctx, &pb.Driver{Name: "张师傅", Phone: "13811110001", LicenseNo: "530123198001011234"})
	if err != nil {
		return err
	}
	if _, err := svc.CreateDriver(ctx, &pb.Driver{Name: "李师傅", Phone: "13811110002", LicenseNo: "530123198505054321"}); err != nil {
		return err
	}

	escort1, err := svc.CreateEscort(ctx, &pb.Escort{Name: "刘芳", Phone: "13822220001", SchoolId: school.Id})
	if err != nil {
		return err
	}
	if _, err := svc.CreateEscort(ctx, &pb.Escort{Name: "赵敏", Phone: "13822220002", SchoolId: school.Id}); err != nil {
		return err
	}

	routeAM, err := svc.CreateRoute(ctx, &pb.CreateRouteRequest{
		SchoolId: school.Id, Name: "一号线·早接", Direction: pb.Direction_DIRECTION_MORNING_PICKUP,
		Stops: []*pb.RouteStop{
			{StopId: stopIDs["县城东站"], Seq: 1, OffsetMin: 0},
			{StopId: stopIDs["河口村委"], Seq: 2, OffsetMin: 8},
			{StopId: stopIDs["石桥镇口"], Seq: 3, OffsetMin: 18},
			{StopId: stopIDs["柳树湾"], Seq: 4, OffsetMin: 28},
			{StopId: stopIDs["学校正门"], Seq: 5, OffsetMin: 40},
		},
	})
	if err != nil {
		return err
	}
	routePM, err := svc.CreateRoute(ctx, &pb.CreateRouteRequest{
		SchoolId: school.Id, Name: "一号线·晚送", Direction: pb.Direction_DIRECTION_AFTERNOON_DROPOFF,
		Stops: []*pb.RouteStop{
			{StopId: stopIDs["学校正门"], Seq: 1, OffsetMin: 0},
			{StopId: stopIDs["柳树湾"], Seq: 2, OffsetMin: 12},
			{StopId: stopIDs["石桥镇口"], Seq: 3, OffsetMin: 22},
			{StopId: stopIDs["河口村委"], Seq: 4, OffsetMin: 32},
			{StopId: stopIDs["县城东站"], Seq: 5, OffsetMin: 40},
		},
	})
	if err != nil {
		return err
	}

	// 学生上下车站点分配（早接上车点 = 晚送下车点）
	assign := map[string]string{
		"王小明": "河口村委", "王小红": "河口村委", "李华": "县城东站", "张倩": "石桥镇口",
		"刘洋": "柳树湾", "陈晨": "石桥镇口", "赵磊": "柳树湾", "孙丽": "河口村委",
	}
	for name, stop := range assign {
		for _, route := range []*pb.Route{routeAM, routePM} {
			if _, err := svc.AssignStudentRoute(ctx, &pb.StudentRouteAssignment{
				StudentId: stuIDs[name], RouteId: route.Id, StopId: stopIDs[stop], Direction: route.Direction,
			}); err != nil {
				return err
			}
		}
	}

	// 当日排班并生成接送任务
	if _, err := svc.CreateSchedule(ctx, &pb.Schedule{
		RouteId: routeAM.Id, VehicleId: veh1.Id, DriverId: driver1.Id, EscortId: escort1.Id,
		ServiceDate: today, Direction: pb.Direction_DIRECTION_MORNING_PICKUP, PlannedDeparture: "07:00",
	}); err != nil {
		return err
	}
	if _, err := svc.CreateSchedule(ctx, &pb.Schedule{
		RouteId: routePM.Id, VehicleId: veh1.Id, DriverId: driver1.Id, EscortId: escort1.Id,
		ServiceDate: today, Direction: pb.Direction_DIRECTION_AFTERNOON_DROPOFF, PlannedDeparture: "17:05",
	}); err != nil {
		return err
	}
	if _, err := svc.GenerateTrips(ctx, &pb.GenerateTripsRequest{Date: today}); err != nil {
		return err
	}
	return nil
}
