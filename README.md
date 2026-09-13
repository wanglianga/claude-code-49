# 县域校车临时改线与学生到站安全服务

## 原始需求

开发县域校车临时改线与学生到站安全服务，可采用 Go、gRPC 和 PostgreSQL。学校维护学生、班级、家长授权、固定站点、接送时段、车辆、司机、随车老师和线路后，服务按每日排班生成接送任务。早晨发车前，司机确认车辆、随车老师确认学生名单，家长端可以看到预计到站时间。学生上车时，随车老师核验学生、站点、临时请假和代送授权；若学生未出现，服务记录等待时间、家长联系次数、是否允许跳站和是否需要学校班主任介入。遇到道路施工、暴雨、车辆故障、临时封路或站点附近拥堵时，司机发起改线，服务重新计算下一站到达时间，并把影响学生、家长通知、学校点名和车辆油耗一起更新。下午放学接送还要处理学生临时参加社团、家长改为自接、兄弟姐妹同车、学生坐错车和到站无人接。所有上车、下车、缺乘、改线、电话联系和异常处置要回到同一趟车的任务中，便于学校确认学生是否安全到校到家，也便于教育局抽查线路履约。服务还要保留车辆离线、随车老师手机没电、家长临时换人接送、乡村道路无信号和学校临时调课等情况的处理轨迹，让一次接送任务从发车前准备一直延续到班主任确认学生状态。

## 技术栈与架构

- **Go 1.25**：单进程同时暴露 gRPC（容器内 `9090`）与 HTTP/JSON 门面（容器内 `8080`，protojson 编解码，与 gRPC 共用同一业务层）
- **gRPC**：5 个服务（`AdminService` / `TripService` / `RerouteService` / `ParentService` / `AuditService`），proto 定义见 `proto/schoolbus/v1/schoolbus.proto`，容器构建时由 protoc 生成代码
- **PostgreSQL 16**：全部业务状态与事件轨迹；schema 内嵌于应用，启动时自动幂等迁移
- **统一事件轨迹**：上车、下车、缺乘、改线、电话联系、异常处置、班主任确认等全部写入 `trip_events`，一趟车一条完整时间线

```
cmd/server        服务入口（gRPC + HTTP 同进程）
cmd/cli           容器内 gRPC 验证客户端
proto/            gRPC 接口定义
internal/service  业务逻辑（直接实现 gRPC 接口）
internal/httpapi  JSON 门面 + 演示台页面
internal/store    连接池与自动迁移（schema.sql）
internal/seed     演示数据（SEED_DEMO=true 时启用）
```

## 一键部署（验证方式 = 宿主 docker compose up）

```bash
cp .env.example .env        # 可选；不建 .env 时使用默认值
docker compose up -d --build
docker compose ps           # 等待 app 健康（healthy）
```

- 应用端口发布到宿主 `${CC_PUBLISH_PORT}`（默认 3049）；**数据库不发布端口**，仅在 compose 内部网络访问。
- 取实际映射端口：`docker compose port app 8080`
- 演示台（浏览器）：`http://localhost:<端口>/`
- 健康检查：`curl http://localhost:<端口>/healthz`
- 容器内验证 gRPC：`docker compose exec app /app/cli -addr localhost:9090 ping`
- 一键业务流回归（50 项断言）：`./scripts/verify.sh http://localhost:<端口>`
- 结束验证：`docker compose down`（加 `-v` 同时清空演示数据）

首次启动自动建表并写入演示数据（`SEED_DEMO=true`），并为**当天**生成早接/晚送两趟任务。

## 测试账号 / 演示数据（逐角色）

系统按角色以业务身份操作（演示环境未做登录鉴权，操作人通过请求字段标明）：

| 角色 | 身份 | 关键标识 |
|---|---|---|
| 司机 | 张师傅（driver_id=1）/ 李师傅（driver_id=2） | 电话 13811110001 / 13811110002 |
| 随车老师 | 刘芳（escort_id=1）/ 赵敏（escort_id=2） | 电话 13822220001 / 13822220002 |
| 班主任 | 王莉（三年级1班）/ 陈强（三年级2班） | 电话 13800000011 / 13800000012 |
| 家长 | 王建国（王小明、王小红之父） | 手机 13900010001（查通知用） |
| 家长 | 李强（李华之父） | 手机 13900020001 |
| 代接授权人 | 李老汉（李华祖父，长期授权）/ 王婆婆（张倩外祖母，长期授权） | 下车核验接站人 |
| 车辆 | 云A·D12345（41 座，0.28L/km）/ 云A·D67890 | vehicle_id=1 / 2 |
| 线路 | 一号线·早接（route_id=1）/ 一号线·晚送（route_id=2） | 5 个站点 |
| 学生 | 王小明/王小红（兄妹，family_id=1001）、李华、张倩、刘洋、陈晨、赵磊、孙丽 | student_id=1..8 |

## 权限绑定（越权拒绝）

- **接送角色绑定**：`ConfirmVehicle` 仅接受排班绑定的 `driver_id`，`ConfirmRoster` 仅接受排班绑定的 `escort_id`；不符返回 `PermissionDenied`（HTTP 403），任务状态不变。
- **家长身份绑定**：`RequestSelfPickup` / `RequestGuardianSwap` 要求 `guardian_phone` 为该学生登记监护人号码；未知号码返回 `PermissionDenied`，学生乘车状态不变。

## 关键业务流（可用演示台按钮或 curl 走通）

以下以早接任务（trip_id=1）为例，HTTP 门面与 gRPC 接口一一对应：

```bash
BASE=http://localhost:3049

# 1) 发车前准备：司机确认车辆 -> 随车老师确认名单 -> 发车（自动生成各站 ETA）
curl -XPOST $BASE/api/trips/1/confirm-vehicle -d '{"driver_id":1,"fuel_liters":118,"note":"车况正常"}'
curl -XPOST $BASE/api/trips/1/confirm-roster  -d '{"escort_id":1,"note":"名单已核对"}'
curl -XPOST $BASE/api/trips/1/start -d '{}'

# 2) 家长端查看预计到站时间
curl $BASE/api/students/1/trip-status

# 3) 上车核验（学生/站点/请假/代送授权）；学生未出现 -> 缺乘处置
curl -XPOST $BASE/api/trips/1/board  -d '{"student_id":1,"stop_id":2,"dropped_off_by":"王建国","actor":"escort:1"}'
curl -XPOST $BASE/api/trips/1/absent -d '{"student_id":3,"stop_id":1,"actor":"escort:1"}'
curl -XPOST $BASE/api/trips/1/contact -d '{"student_id":3,"phone":"13900020001","result":"无人接听","actor":"escort:1"}'
curl -XPOST $BASE/api/wait-records/update -d '{"wait_record_id":1,"wait_seconds":300,"skip_allowed":true,"teacher_involved":true,"resolved":true,"resolution":"家长同意跳站，班主任已介入"}'

# 4) 临时改线（封路）：重算 ETA + 影响学生 + 家长通知 + 学校点名 + 车辆油耗
curl -XPOST $BASE/api/trips/1/reroute -d '{"reason":"REROUTE_ROAD_CLOSURE","detail":"前方临时封路","added_delay_min":15,"extra_km":6.5,"initiated_by":"driver:1"}'

# 5) 到站无人接 / 坐错车 / 兄弟姐妹同车（晚送任务 trip_id=2 同样适用）
curl -XPOST $BASE/api/trips/2/alight -d '{"student_id":5,"stop_id":4,"receiver_name":"","actor":"escort:1"}'
curl -XPOST $BASE/api/trips/1/sibling-ride -d '{"student_ids":[1,2],"note":"兄妹同车","actor":"escort:1"}'

# 6) 下午场景：社团 / 自接 / 临时换人接送 / 请假
curl -XPOST $BASE/api/students/6/club -d '{"club_name":"足球社团"}'
curl -XPOST $BASE/api/students/4/self-pickup -d '{"guardian_phone":"13900030001"}'
curl -XPOST $BASE/api/students/7/guardian-swap -d '{"temp_name":"王叔叔","temp_phone":"13966668888","relation":"邻居","guardian_phone":"13900060001"}'
curl -XPOST $BASE/api/students/leave -d '{"student_id":8,"reason":"感冒发烧","reported_by":"guardian:13900070001"}'

# 7) 降级与轨迹：车辆离线 / 无信号 -> 离线事件补录；异常处置闭环
curl -XPOST $BASE/api/trips/1/exceptions -d '{"type":"EX_NO_SIGNAL","detail":"柳树湾段无信号","reported_by":"driver:1"}'
curl -XPOST $BASE/api/trips/1/sync-offline -d '{"actor":"escort:1","events":[{"event_type":"board","student_id":2,"stop_id":2,"note":"无信号补录"}]}'
curl -XPOST $BASE/api/exceptions/1/resolve -d '{"resolution":"驶出无信号区，恢复在线","resolved_by":"driver:1"}'

# 8) 完成任务 -> 班主任确认学生状态（全部确认后任务自动闭环）
curl -XPOST $BASE/api/trips/1/complete -d '{}'
curl -XPOST $BASE/api/trips/1/homeroom-confirm -d '{"student_id":1,"teacher_name":"王莉","status":"HM_SAFE_AT_SCHOOL"}'

# 9) 学校与教育局视角：全程轨迹 / 学校点名 / 线路履约抽查
curl $BASE/api/trips/1/trace
curl $BASE/api/trips/1/rollcall
curl $BASE/api/audit/routes/1/fulfillment
```

## 接口一览

- **gRPC**（容器内 9090）：`AdminService`（基础数据维护）、`TripService`（任务全生命周期）、`RerouteService`（改线）、`ParentService`（家长端）、`AuditService`（履约/点名）；已开启 server reflection
- **HTTP/JSON**（发布端口）：`/api/admin/*`、`/api/trips/*`、`/api/students/*`、`/api/notifications`、`/api/audit/*`，GET/POST 语义与 gRPC 方法一一对应；`/` 为演示台页面，`/healthz` 为健康检查
