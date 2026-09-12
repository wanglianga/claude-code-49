package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	pb "schoolbus/gen/schoolbusv1"
)

// ---------------------------------------------------------------------------
// RerouteService：临时改线（道路施工/暴雨/车辆故障/临时封路/站点拥堵）
// ---------------------------------------------------------------------------

func (s *Service) InitiateReroute(ctx context.Context, req *pb.InitiateRerouteRequest) (*pb.RerouteResult, error) {
	t, err := s.getTrip(ctx, s.db, req.TripId)
	if err != nil {
		return nil, err
	}
	if t.Status != pb.TripStatus_TRIP_STATUS_IN_PROGRESS {
		return nil, errPrecond("只有在途任务可以发起改线")
	}
	if req.Reason == pb.RerouteReason_REROUTE_REASON_UNSPECIFIED {
		return nil, errInvalid("改线原因不能为空")
	}
	if req.AddedDelayMin == 0 && req.ExtraKm == 0 && len(req.SkipStopIds) == 0 {
		return nil, errInvalid("改线至少需要：增加延误分钟、绕行里程或跳过站点之一")
	}

	result := &pb.RerouteResult{}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		fromSeq := t.CurrentSeq
		// 1) 未到达站点 ETA 整体顺延
		if req.AddedDelayMin != 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE stop_etas SET current_eta = current_eta + ($2::int || ' minutes')::interval
				WHERE trip_id=$1 AND status='pending' AND seq >= $3`,
				req.TripId, req.AddedDelayMin, fromSeq); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE trips SET delay_min = delay_min + $2 WHERE id=$1`,
				req.TripId, req.AddedDelayMin); err != nil {
				return err
			}
		}
		// 2) 跳过站点：ETA 标记 skipped，站点学生改为 skipped 并通知
		skipSet := map[int64]bool{}
		for _, sid := range req.SkipStopIds {
			skipSet[sid] = true
		}
		if len(req.SkipStopIds) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE stop_etas SET status='skipped'
				WHERE trip_id=$1 AND stop_id = ANY($2) AND status='pending'`,
				req.TripId, req.SkipStopIds); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT ts.student_id, st.name, sp.name FROM trip_students ts
				JOIN students st ON st.id=ts.student_id
				JOIN stops sp ON sp.id=ts.stop_id
				WHERE ts.trip_id=$1 AND ts.stop_id = ANY($2) AND ts.status IN ('expected')`,
				req.TripId, req.SkipStopIds)
			if err != nil {
				return err
			}
			type skippedStu struct {
				id       int64
				name     string
				stopName string
			}
			var skipped []skippedStu
			for rows.Next() {
				var x skippedStu
				if err := rows.Scan(&x.id, &x.name, &x.stopName); err != nil {
					rows.Close()
					return err
				}
				skipped = append(skipped, x)
			}
			rows.Close()
			for _, x := range skipped {
				if _, err := tx.Exec(ctx, `
					UPDATE trip_students SET status='skipped', note='改线跳过站点，请与学校联系'
					WHERE trip_id=$1 AND student_id=$2`, req.TripId, x.id); err != nil {
					return err
				}
				if _, err := s.emitEvent(ctx, tx, req.TripId, EvSkipStopAuthorized, eventOpts{
					studentID: &x.id,
					actor:     req.InitiatedBy,
					detail:    map[string]any{"reason": "reroute_skip_stop", "stop_name": x.stopName},
				}); err != nil {
					return err
				}
				if _, err := s.notifyStudent(ctx, tx, req.TripId, x.id, "reroute",
					fmt.Sprintf("【校车】因%s，本次班车不再停靠「%s」，%s 的接送请与学校联系另行安排。",
						RerouteReasonLabel(req.Reason), x.stopName, x.name)); err != nil {
					return err
				}
			}
		}

		// 3) 车辆油耗：绕行里程 * 百公里单耗，扣减油量并记日志
		var fuelDelta float64
		if req.ExtraKm > 0 {
			if err := tx.QueryRow(ctx, `SELECT fuel_per_km FROM vehicles WHERE id=$1`, t.VehicleId).Scan(&fuelDelta); err != nil {
				return err
			}
			fuelDelta = fuelDelta * req.ExtraKm
			if _, err := tx.Exec(ctx, `UPDATE vehicles SET fuel_liters = GREATEST(fuel_liters - $2, 0) WHERE id=$1`,
				t.VehicleId, fuelDelta); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO fuel_logs (trip_id, vehicle_id, delta_liters, reason)
				VALUES ($1,$2,$3,$4)`,
				req.TripId, t.VehicleId, -fuelDelta,
				fmt.Sprintf("改线绕行 %.1f 公里（%s）", req.ExtraKm, RerouteReasonLabel(req.Reason))); err != nil {
				return err
			}
			if _, err := s.emitEvent(ctx, tx, req.TripId, EvFuelUpdated, eventOpts{
				actor: req.InitiatedBy,
				detail: map[string]any{
					"extra_km": req.ExtraKm, "fuel_delta_liters": fuelDelta,
				},
			}); err != nil {
				return err
			}
		}

		// 4) 影响学生：未到达站点等车的 + 已在车上的
		affectedRows, err := tx.Query(ctx, `
			SELECT DISTINCT ts.student_id FROM trip_students ts
			LEFT JOIN stop_etas e ON e.trip_id=ts.trip_id AND e.stop_id=ts.stop_id
			WHERE ts.trip_id=$1 AND (
			      ts.status='boarded'
			   OR (ts.status='expected' AND e.status='pending' AND e.seq >= $2)
			)`, req.TripId, fromSeq)
		if err != nil {
			return err
		}
		var affectedIDs []int64
		for affectedRows.Next() {
			var id int64
			if err := affectedRows.Scan(&id); err != nil {
				affectedRows.Close()
				return err
			}
			affectedIDs = append(affectedIDs, id)
		}
		affectedRows.Close()

		// 5) 改线记录
		skipJSON, _ := json.Marshal(req.SkipStopIds)
		var rerouteID int64
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO reroutes (trip_id, reason, detail, from_seq, added_delay_min, extra_km, skip_stop_ids, affected_count, fuel_delta_liters, initiated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id, created_at`,
			req.TripId, rerouteReasonToDB(req.Reason), req.Detail, fromSeq,
			req.AddedDelayMin, req.ExtraKm, skipJSON, len(affectedIDs), fuelDelta, req.InitiatedBy).
			Scan(&rerouteID, &createdAt); err != nil {
			return err
		}
		result.Reroute = &pb.Reroute{
			Id: rerouteID, TripId: req.TripId, Reason: req.Reason, Detail: req.Detail,
			FromSeq: fromSeq, AddedDelayMin: req.AddedDelayMin, ExtraKm: req.ExtraKm,
			SkipStopIds: req.SkipStopIds, AffectedCount: int32(len(affectedIDs)),
			FuelDeltaLiters: fuelDelta, InitiatedBy: req.InitiatedBy, CreatedAt: tsVal(createdAt),
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvRerouteInitiated, eventOpts{
			actor: req.InitiatedBy,
			detail: map[string]any{
				"reroute_id": rerouteID, "reason": rerouteReasonToDB(req.Reason),
				"detail": req.Detail, "added_delay_min": req.AddedDelayMin,
				"extra_km": req.ExtraKm, "skip_stop_ids": req.SkipStopIds,
				"affected_count": len(affectedIDs),
			},
		}); err != nil {
			return err
		}

		// 6) 重算后的 ETA 事件 + 家长通知 + 学校点名同步
		etas, err := s.loadStopEtas(ctx, tx, req.TripId)
		if err != nil {
			return err
		}
		result.UpdatedEtas = etas
		etaDetail := map[string]any{}
		for _, e := range etas {
			if e.CurrentEta != nil {
				etaDetail[fmt.Sprintf("stop_%d(%s)", e.StopId, e.StopName)] = e.CurrentEta.AsTime().Format("15:04")
			}
		}
		if _, err := s.emitEvent(ctx, tx, req.TripId, EvEtaUpdated, eventOpts{
			actor:  req.InitiatedBy,
			detail: etaDetail,
		}); err != nil {
			return err
		}

		// 每个受影响学生的家长收到新的预计到站时间
		for _, sid := range affectedIDs {
			ts, err := s.getTripStudent(ctx, tx, req.TripId, sid)
			if err != nil {
				return err
			}
			var etaText string
			for _, e := range etas {
				if e.StopId == ts.StopId && e.CurrentEta != nil {
					etaText = e.CurrentEta.AsTime().In(s.loc).Format("15:04")
				}
			}
			if etaText == "" {
				etaText = "待定"
			}
			content := fmt.Sprintf("【校车】因%s，%s 所乘班车临时改线，站点「%s」新的预计到站时间 %s。",
				RerouteReasonLabel(req.Reason), ts.StudentName, ts.StopName, etaText)
			ns, err := s.notifyStudent(ctx, tx, req.TripId, sid, "reroute", content)
			if err != nil {
				return err
			}
			result.Notifications = append(result.Notifications, ns...)
			result.AffectedStudents = append(result.AffectedStudents, ts)
		}
		if len(result.Notifications) > 0 {
			if _, err := s.emitEvent(ctx, tx, req.TripId, EvNotificationSent, eventOpts{
				actor:  "system",
				detail: map[string]any{"count": len(result.Notifications), "type": "reroute"},
			}); err != nil {
				return err
			}
		}
		// 学校点名同步：给相关班主任发点名变更通知
		clsRows, err := tx.Query(ctx, `
			SELECT DISTINCT c.homeroom_teacher, c.homeroom_phone FROM trip_students ts
			JOIN students st ON st.id=ts.student_id JOIN classes c ON c.id=st.class_id
			WHERE ts.trip_id=$1 AND ts.student_id = ANY($2) AND c.homeroom_phone<>''`,
			req.TripId, affectedIDs)
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
			if _, err := s.insertNotification(ctx, tx, req.TripId, 0, tc.phone, "rollcall_update",
				fmt.Sprintf("【校车】%s 因%s临时改线，%d 名学生到站时间变更，请班主任 %s 更新点名。",
					t.RouteName, RerouteReasonLabel(req.Reason), len(affectedIDs), tc.name)); err != nil {
				return err
			}
		}

		// 7) 返回车辆剩余油量
		return tx.QueryRow(ctx, `SELECT fuel_liters FROM vehicles WHERE id=$1`, t.VehicleId).Scan(&result.VehicleFuelLiters)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) ListReroutes(ctx context.Context, req *pb.ListReroutesRequest) (*pb.ListReroutesResponse, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, trip_id, reason, detail, from_seq, added_delay_min, extra_km,
		       skip_stop_ids, affected_count, fuel_delta_liters, initiated_by, created_at
		FROM reroutes WHERE trip_id=$1 ORDER BY id`, req.TripId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &pb.ListReroutesResponse{}
	for rows.Next() {
		var r pb.Reroute
		var reason string
		var skipJSON []byte
		var createdAt time.Time
		if err := rows.Scan(&r.Id, &r.TripId, &reason, &r.Detail, &r.FromSeq, &r.AddedDelayMin,
			&r.ExtraKm, &skipJSON, &r.AffectedCount, &r.FuelDeltaLiters, &r.InitiatedBy, &createdAt); err != nil {
			return nil, err
		}
		r.Reason = rerouteReasonFromDB(reason)
		_ = json.Unmarshal(skipJSON, &r.SkipStopIds)
		r.CreatedAt = tsVal(createdAt)
		out.Reroutes = append(out.Reroutes, &r)
	}
	return out, rows.Err()
}
