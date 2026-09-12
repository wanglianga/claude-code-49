#!/usr/bin/env bash
# 关键业务流端到端验证：发车前准备 -> 上下车核验 -> 缺乘处置 -> 改线 ->
# 下午场景 -> 降级/异常 -> 完成 -> 班主任确认 -> 履约审计
# 用法: ./scripts/verify.sh [BASE_URL]
set -u
BASE="${1:-http://host.docker.internal:3049}"
PASS=0; FAIL=0

req() { # req METHOD PATH [JSON]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -s -X "$method" "$BASE$path" -H 'Content-Type: application/json' -d "$body"
  else
    curl -s -X "$method" "$BASE$path"
  fi
}

check() { # check DESC HAYSTACK NEEDLE [NEEDLE...]
  local desc="$1" hay="$2"; shift 2
  local needle ok=1
  for needle in "$@"; do
    case "$hay" in *"$needle"*) ;; *) ok=0;; esac
  done
  if [ "$ok" = 1 ]; then PASS=$((PASS+1)); echo "  PASS  $desc";
  else FAIL=$((FAIL+1)); echo "  FAIL  $desc"; echo "        输出: $(echo "$hay" | head -c 400)"; fi
}

jsonval() { # 提取简单字段（int64 在 protojson 中为字符串形式）
  echo "$1" | tr ',' '\n' | grep -o "\"$2\":\"\?[0-9]*" | head -1 | grep -o '[0-9]*$'
}

echo "== 0. 健康检查与演示数据 =="
check "healthz" "$(req GET /healthz)" '"status":"ok"'
OV=$(req GET /api/overview)
check "演示数据已就绪" "$OV" '"students":"8"' '"routes":"2"' '"today_trips"'

echo "== 1. 早接任务：发车前准备 =="
R=$(req POST /api/trips/1/confirm-vehicle '{"driver_id":1,"fuel_liters":118,"note":"车况正常，油量充足"}')
check "司机确认车辆" "$R" 'TRIP_STATUS_PREPARING'
R=$(req POST /api/trips/1/confirm-roster '{"escort_id":1,"note":"名单已核对"}')
check "随车老师确认名单" "$R" 'TRIP_STATUS_READY'
R=$(req POST /api/trips/1/start '{}')
check "发车" "$R" 'TRIP_STATUS_IN_PROGRESS' 'actual_departure'
R=$(req POST /api/trips/1/arrive-stop '{"stop_id":1}')
check "到站登记(县城东站)" "$R" '"status":"arrived"'
R=$(req POST /api/trips/1/arrive-stop '{"stop_id":2}')
check "到站登记(河口村委)" "$R" '"current_seq":2'

echo "== 2. 家长端 ETA =="
R=$(req GET "/api/students/1/trip-status")
check "家长查看预计到站时间" "$R" '"stop_eta"' 'current_eta' '河口村委'

echo "== 3. 上车核验 =="
R=$(req POST /api/trips/1/board '{"student_id":1,"stop_id":2,"dropped_off_by":"王建国","actor":"escort:1"}')
check "正常上车(授权送站人)" "$R" 'TS_BOARDED'
R=$(req POST /api/trips/1/board '{"student_id":2,"stop_id":2,"actor":"escort:1"}')
check "妹妹上车" "$R" 'TS_BOARDED'
R=$(req POST /api/trips/1/board '{"student_id":4,"stop_id":3,"dropped_off_by":"陌生人","actor":"escort:1"}')
check "送站人未授权->警告并放行" "$R" 'TS_BOARDED' '不在授权名单'
R=$(req POST /api/trips/1/board '{"student_id":5,"stop_id":4,"actor":"escort:1"}')
check "学生5上车" "$R" 'TS_BOARDED'
R=$(req POST /api/trips/1/board '{"student_id":7,"stop_id":4,"actor":"escort:1"}')
check "学生7上车" "$R" 'TS_BOARDED'
R=$(req POST /api/trips/1/board '{"student_id":999,"stop_id":2,"actor":"escort:1"}')
check "坐错车识别" "$R" '"wrong_bus":true' 'EX_WRONG_BUS'

echo "== 4. 缺乘处置 =="
R=$(req POST /api/trips/1/absent '{"student_id":3,"stop_id":1,"actor":"escort:1"}')
WID=$(jsonval "$R" id)
check "标记缺乘+等待记录" "$R" '"resolved":false' '李华'
R=$(req POST /api/trips/1/contact '{"student_id":3,"phone":"13900020001","result":"无人接听","actor":"escort:1"}')
check "第1次联系家长" "$R" '"contacted_count":1'
R=$(req POST /api/trips/1/contact '{"student_id":3,"phone":"13900020001","result":"家长说孩子已请假","actor":"escort:1"}')
check "第2次联系家长" "$R" '"contacted_count":2'
R=$(req POST /api/wait-records/update "{\"wait_record_id\":$WID,\"wait_seconds\":300,\"skip_allowed\":true,\"teacher_involved\":true,\"resolved\":true,\"resolution\":\"家长同意跳站，班主任介入\"}")
check "允许跳站+班主任介入" "$R" '"skip_allowed":true' '"teacher_involved":true' '"resolved":true'
R=$(req GET /api/trips/1)
check "学生3状态=跳站" "$R" 'TS_SKIPPED'

echo "== 5. 临时改线（封路） =="
R=$(req POST /api/trips/1/reroute '{"reason":"REROUTE_ROAD_CLOSURE","detail":"前方临时封路，绕行乡道","added_delay_min":15,"extra_km":6.5,"initiated_by":"driver:1"}')
check "改线:ETA重算" "$R" '"updated_etas"' '"added_delay_min":15'
check "改线:影响学生" "$R" '"affected_students"' '王小明'
check "改线:家长通知" "$R" '"notifications"' '临时改线'
check "改线:油耗扣减(118-1.82)" "$R" '"vehicle_fuel_liters":116.18'
R=$(req GET /api/notifications?phone=13900010001)
check "家长收到改线通知" "$R" 'reroute' '新的预计到站时间'

echo "== 6. 降级与异常轨迹 =="
R=$(req POST /api/trips/1/exceptions '{"type":"EX_NO_SIGNAL","detail":"柳树湾段无信号","reported_by":"driver:1"}')
EX1=$(jsonval "$R" id)
check "上报无信号异常" "$R" 'EX_NO_SIGNAL' '"status":"EX_STATUS_OPEN"'
R=$(req GET /api/trips/1)
check "任务进入离线降级" "$R" '"offline_mode":true'
R=$(req POST /api/trips/1/sync-offline '{"actor":"escort:1","events":[{"event_type":"board","student_id":6,"stop_id":3,"note":"无信号期间上车，恢复后补录"}]}')
check "离线事件补录" "$R" '"applied":1'
R=$(req POST /api/exceptions/$EX1/resolve '{"resolution":"驶出无信号区","resolved_by":"driver:1"}')
check "异常闭环" "$R" 'EX_STATUS_RESOLVED'
R=$(req POST /api/trips/1/exceptions '{"type":"EX_ESCORT_PHONE_DEAD","detail":"随车老师手机没电，暂用司机手机","reported_by":"escort:1"}')
EX2=$(jsonval "$R" id)
check "手机没电异常" "$R" 'EX_ESCORT_PHONE_DEAD'
R=$(req POST /api/exceptions/$EX2/resolve '{"resolution":"使用车载充电恢复","resolved_by":"escort:1"}')
check "手机没电处置完成" "$R" 'EX_STATUS_RESOLVED'

echo "== 7. 学校临时调课（晚送任务） =="
R=$(req POST /api/trips/2/exceptions '{"type":"EX_CLASS_RESCHEDULED","detail":"教育局临时会议","reported_by":"school","new_departure":"17:40"}')
check "调课异常登记" "$R" 'EX_CLASS_RESCHEDULED'
R=$(req GET /api/trips/2)
check "晚送发车时间调整" "$R" '"planned_departure":"17:40"'

echo "== 8. 下午场景 =="
R=$(req POST /api/students/6/club '{"club_name":"足球社团"}')
check "学生临时参加社团" "$R" 'TS_CLUB_ACTIVITY'
R=$(req POST /api/students/4/self-pickup '{"guardian_phone":"13900030001","note":"今天妈妈自己接"}')
check "家长改为自接" "$R" 'TS_SELF_PICKUP'
R=$(req POST /api/students/7/guardian-swap '{"temp_name":"王叔叔","temp_phone":"13966668888","relation":"邻居","guardian_phone":"13900060001"}')
check "家长临时换人接送(临时授权)" "$R" '"auth_type":"temporary"' '王叔叔'
R=$(req POST /api/students/leave '{"student_id":8,"reason":"感冒发烧","reported_by":"guardian:13900070001"}')
check "临时请假" "$R" '"leave_date"'
R=$(req POST /api/trips/2/sibling-ride '{"student_ids":[1,2],"note":"兄妹同车回家","actor":"escort:1"}')
check "兄弟姐妹同车登记" "$R" 'sibling_ride'

echo "== 9. 晚送任务执行 =="
req POST /api/trips/2/confirm-vehicle '{"driver_id":1,"fuel_liters":116}' > /dev/null
req POST /api/trips/2/confirm-roster '{"escort_id":1}' > /dev/null
R=$(req POST /api/trips/2/start '{}')
check "晚送发车" "$R" 'TRIP_STATUS_IN_PROGRESS'
req POST /api/trips/2/arrive-stop '{"stop_id":5}' > /dev/null
for sid in 1 2 5 7; do req POST /api/trips/2/board "{\"student_id\":$sid,\"stop_id\":5,\"actor\":\"escort:1\"}" > /dev/null; done
R=$(req POST /api/trips/2/board '{"student_id":6,"stop_id":5,"actor":"escort:1"}')
check "社团学生不能上车" "$R" '社团'
R=$(req POST /api/trips/2/alight '{"student_id":5,"stop_id":4,"receiver_name":"","actor":"escort:1"}')
check "到站无人接->随车滞留" "$R" 'TS_NO_RECEIVER' 'EX_NO_RECEIVER'
R=$(req POST /api/trips/2/alight '{"student_id":7,"stop_id":4,"receiver_name":"王叔叔","actor":"escort:1"}')
check "临时授权人接走" "$R" 'TS_ALIGHTED' '"receiver_authorized":true'
R=$(req POST /api/trips/2/alight '{"student_id":5,"stop_id":4,"receiver_name":"刘军","actor":"escort:1"}')
check "家长赶到后接走" "$R" 'TS_ALIGHTED'
req POST /api/trips/2/alight '{"student_id":1,"stop_id":2,"receiver_name":"王建国","actor":"escort:1"}' > /dev/null
req POST /api/trips/2/alight '{"student_id":2,"stop_id":2,"receiver_name":"李秀英","actor":"escort:1"}' > /dev/null
# 李华晚送也未出现：缺乘 -> 联系 -> 跳站
R=$(req POST /api/trips/2/absent '{"student_id":3,"stop_id":1,"actor":"escort:1"}')
WID2=$(jsonval "$R" id)
req POST /api/trips/2/contact '{"student_id":3,"phone":"13900020001","result":"家长说今天自己接","actor":"escort:1"}' > /dev/null
R=$(req POST /api/wait-records/update "{\"wait_record_id\":$WID2,\"wait_seconds\":180,\"skip_allowed\":true,\"teacher_involved\":false,\"resolved\":true,\"resolution\":\"家长自接，允许跳站\"}")
check "晚送缺乘跳站处置" "$R" '"resolved":true'
R=$(req POST /api/trips/2/complete '{}')
check "晚送任务完成" "$R" 'TRIP_STATUS_COMPLETED'

echo "== 10. 早接送校+任务闭环 =="
for sid in 1 2 4 5 6 7; do req POST /api/trips/1/alight "{\"student_id\":$sid,\"stop_id\":5,\"receiver_name\":\"门卫张叔\",\"actor\":\"escort:1\",\"confirm_unauthorized\":true}" > /dev/null; done
R=$(req POST /api/trips/1/complete '{}')
check "早接任务完成" "$R" 'TRIP_STATUS_COMPLETED'
for sid in 1 2 3 4 5 6 7 8; do
  R=$(req POST /api/trips/1/homeroom-confirm "{\"student_id\":$sid,\"teacher_name\":\"王莉\",\"status\":\"HM_SAFE_AT_SCHOOL\"}")
done
check "班主任全部确认->任务闭环" "$R" 'TRIP_STATUS_CLOSED'

echo "== 11. 学校与教育局视角 =="
R=$(req GET /api/trips/1/trace)
check "全程轨迹(>20事件)" "$R" 'vehicle_confirmed' 'student_boarded' 'parent_contacted' 'reroute_initiated' 'offline_sync' 'homeroom_confirmed' 'trip_closed'
R=$(req GET /api/trips/1/rollcall)
check "学校点名" "$R" 'HM_SAFE_AT_SCHOOL' '王小明'
R=$(req GET /api/audit/routes/1/fulfillment)
check "教育局履约抽查" "$R" '"reroute_count":1' '"stops_total":5' '"stops_arrived":2' 'on_time_departure'
R=$(req GET /api/trips/2/trace)
check "晚送轨迹含下午场景" "$R" 'club_activity' 'self_pickup' 'guardian_swapped' 'sibling_ride' 'no_receiver'

echo
echo "==================================="
echo "验证结果: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = 0 ]
