-- 县域校车临时改线与学生到站安全服务 数据库结构
-- 所有表幂等创建（IF NOT EXISTS），应用启动时自动执行。

CREATE TABLE IF NOT EXISTS schools (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL,
  address     TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS classes (
  id               BIGSERIAL PRIMARY KEY,
  school_id        BIGINT NOT NULL REFERENCES schools(id),
  name             TEXT NOT NULL,
  grade            TEXT NOT NULL DEFAULT '',
  homeroom_teacher TEXT NOT NULL DEFAULT '', -- 班主任
  homeroom_phone   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS students (
  id         BIGSERIAL PRIMARY KEY,
  class_id   BIGINT NOT NULL REFERENCES classes(id),
  name       TEXT NOT NULL,
  student_no TEXT NOT NULL DEFAULT '',
  gender     TEXT NOT NULL DEFAULT '',
  family_id  BIGINT NOT NULL DEFAULT 0, -- 相同 family_id 为兄弟姐妹
  status     TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS guardians (
  id         BIGSERIAL PRIMARY KEY,
  student_id BIGINT NOT NULL REFERENCES students(id),
  name       TEXT NOT NULL,
  relation   TEXT NOT NULL DEFAULT '',
  phone      TEXT NOT NULL,
  is_primary BOOLEAN NOT NULL DEFAULT false
);

-- 家长授权 / 代送授权 / 临时换人授权
CREATE TABLE IF NOT EXISTS pickup_authorizations (
  id               BIGSERIAL PRIMARY KEY,
  student_id       BIGINT NOT NULL REFERENCES students(id),
  authorized_name  TEXT NOT NULL,
  authorized_phone TEXT NOT NULL DEFAULT '',
  relation         TEXT NOT NULL DEFAULT '',
  auth_type        TEXT NOT NULL DEFAULT 'regular', -- regular | temporary
  valid_date       DATE,                            -- temporary 生效日期
  created_by       TEXT NOT NULL DEFAULT '',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS stops (
  id        BIGSERIAL PRIMARY KEY,
  school_id BIGINT NOT NULL REFERENCES schools(id),
  name      TEXT NOT NULL,
  address   TEXT NOT NULL DEFAULT '',
  latitude  DOUBLE PRECISION NOT NULL DEFAULT 0,
  longitude DOUBLE PRECISION NOT NULL DEFAULT 0
);

-- 接送时段
CREATE TABLE IF NOT EXISTS time_windows (
  id         BIGSERIAL PRIMARY KEY,
  school_id  BIGINT NOT NULL REFERENCES schools(id),
  name       TEXT NOT NULL,
  direction  TEXT NOT NULL, -- morning_pickup | afternoon_dropoff
  start_time TEXT NOT NULL, -- HH:MM
  end_time   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vehicles (
  id           BIGSERIAL PRIMARY KEY,
  plate_no     TEXT NOT NULL UNIQUE,
  capacity     INT NOT NULL DEFAULT 30,
  fuel_liters  DOUBLE PRECISION NOT NULL DEFAULT 0,
  fuel_per_km  DOUBLE PRECISION NOT NULL DEFAULT 0.25,
  status       TEXT NOT NULL DEFAULT 'available'
);

CREATE TABLE IF NOT EXISTS drivers (
  id         BIGSERIAL PRIMARY KEY,
  name       TEXT NOT NULL,
  phone      TEXT NOT NULL DEFAULT '',
  license_no TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS escorts (
  id        BIGSERIAL PRIMARY KEY,
  name      TEXT NOT NULL,
  phone     TEXT NOT NULL DEFAULT '',
  school_id BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS routes (
  id        BIGSERIAL PRIMARY KEY,
  school_id BIGINT NOT NULL REFERENCES schools(id),
  name      TEXT NOT NULL,
  direction TEXT NOT NULL, -- morning_pickup | afternoon_dropoff
  active    BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE IF NOT EXISTS route_stops (
  id         BIGSERIAL PRIMARY KEY,
  route_id   BIGINT NOT NULL REFERENCES routes(id),
  stop_id    BIGINT NOT NULL REFERENCES stops(id),
  seq        INT NOT NULL,
  offset_min INT NOT NULL DEFAULT 0, -- 距发车的计划分钟数
  UNIQUE(route_id, seq)
);

-- 学生-线路-站点分配
CREATE TABLE IF NOT EXISTS student_routes (
  id         BIGSERIAL PRIMARY KEY,
  student_id BIGINT NOT NULL REFERENCES students(id),
  route_id   BIGINT NOT NULL REFERENCES routes(id),
  stop_id    BIGINT NOT NULL REFERENCES stops(id),
  direction  TEXT NOT NULL,
  UNIQUE(student_id, route_id, direction)
);

-- 临时请假
CREATE TABLE IF NOT EXISTS leave_records (
  id          BIGSERIAL PRIMARY KEY,
  student_id  BIGINT NOT NULL REFERENCES students(id),
  leave_date  DATE NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  reported_by TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(student_id, leave_date)
);

-- 每日排班
CREATE TABLE IF NOT EXISTS schedules (
  id                BIGSERIAL PRIMARY KEY,
  route_id          BIGINT NOT NULL REFERENCES routes(id),
  vehicle_id        BIGINT NOT NULL REFERENCES vehicles(id),
  driver_id         BIGINT NOT NULL REFERENCES drivers(id),
  escort_id         BIGINT NOT NULL REFERENCES escorts(id),
  service_date      DATE NOT NULL,
  direction         TEXT NOT NULL,
  planned_departure TEXT NOT NULL, -- HH:MM
  status            TEXT NOT NULL DEFAULT 'planned',
  UNIQUE(route_id, service_date, direction)
);

-- 接送任务（一趟车）：从发车前准备到班主任确认的完整生命周期
CREATE TABLE IF NOT EXISTS trips (
  id                   BIGSERIAL PRIMARY KEY,
  schedule_id          BIGINT NOT NULL REFERENCES schedules(id),
  route_id             BIGINT NOT NULL,
  vehicle_id           BIGINT NOT NULL,
  driver_id            BIGINT NOT NULL,
  escort_id            BIGINT NOT NULL,
  service_date         DATE NOT NULL,
  direction            TEXT NOT NULL,
  status               TEXT NOT NULL DEFAULT 'scheduled',
  planned_departure    TEXT NOT NULL,
  vehicle_confirmed_at TIMESTAMPTZ,
  vehicle_confirm_note TEXT NOT NULL DEFAULT '',
  roster_confirmed_at  TIMESTAMPTZ,
  actual_departure     TIMESTAMPTZ,
  actual_arrival       TIMESTAMPTZ,
  current_seq          INT NOT NULL DEFAULT 0,
  offline_mode         BOOLEAN NOT NULL DEFAULT false, -- 车辆离线/无信号降级
  delay_min            INT NOT NULL DEFAULT 0,         -- 累计改线延误
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS trip_students (
  id             BIGSERIAL PRIMARY KEY,
  trip_id        BIGINT NOT NULL REFERENCES trips(id),
  student_id     BIGINT NOT NULL REFERENCES students(id),
  stop_id        BIGINT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'expected',
  boarded_at     TIMESTAMPTZ,
  alighted_at    TIMESTAMPTZ,
  board_stop_id  BIGINT,
  alight_stop_id BIGINT,
  note           TEXT NOT NULL DEFAULT '',
  UNIQUE(trip_id, student_id)
);

-- 每站预计到站时间
CREATE TABLE IF NOT EXISTS stop_etas (
  id          BIGSERIAL PRIMARY KEY,
  trip_id     BIGINT NOT NULL REFERENCES trips(id),
  stop_id     BIGINT NOT NULL,
  seq         INT NOT NULL,
  planned_eta TIMESTAMPTZ,
  current_eta TIMESTAMPTZ,
  status      TEXT NOT NULL DEFAULT 'pending', -- pending | arrived | skipped
  arrived_at  TIMESTAMPTZ,
  UNIQUE(trip_id, stop_id)
);

-- 统一事件轨迹：上车/下车/缺乘/改线/电话联系/异常处置全部回到同一趟车
CREATE TABLE IF NOT EXISTS trip_events (
  id          BIGSERIAL PRIMARY KEY,
  trip_id     BIGINT NOT NULL REFERENCES trips(id),
  event_type  TEXT NOT NULL,
  student_id  BIGINT,
  stop_id     BIGINT,
  actor       TEXT NOT NULL DEFAULT '',
  detail      JSONB NOT NULL DEFAULT '{}',
  offline     BOOLEAN NOT NULL DEFAULT false,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  synced_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_trip_events_trip ON trip_events(trip_id, id);

-- 缺乘等待记录
CREATE TABLE IF NOT EXISTS wait_records (
  id               BIGSERIAL PRIMARY KEY,
  trip_id          BIGINT NOT NULL REFERENCES trips(id),
  student_id       BIGINT NOT NULL,
  stop_id          BIGINT NOT NULL,
  wait_started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  wait_seconds     INT NOT NULL DEFAULT 0,
  contacted_count  INT NOT NULL DEFAULT 0,
  skip_allowed     BOOLEAN NOT NULL DEFAULT false,
  teacher_involved BOOLEAN NOT NULL DEFAULT false,
  resolved         BOOLEAN NOT NULL DEFAULT false,
  resolution       TEXT NOT NULL DEFAULT ''
);

-- 家长电话联系记录
CREATE TABLE IF NOT EXISTS contact_logs (
  id             BIGSERIAL PRIMARY KEY,
  trip_id        BIGINT NOT NULL REFERENCES trips(id),
  student_id     BIGINT NOT NULL,
  wait_record_id BIGINT,
  phone          TEXT NOT NULL DEFAULT '',
  result         TEXT NOT NULL DEFAULT '',
  actor          TEXT NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 临时改线
CREATE TABLE IF NOT EXISTS reroutes (
  id                BIGSERIAL PRIMARY KEY,
  trip_id           BIGINT NOT NULL REFERENCES trips(id),
  reason            TEXT NOT NULL,
  detail            TEXT NOT NULL DEFAULT '',
  from_seq          INT NOT NULL DEFAULT 0,
  added_delay_min   INT NOT NULL DEFAULT 0,
  extra_km          DOUBLE PRECISION NOT NULL DEFAULT 0,
  skip_stop_ids     JSONB NOT NULL DEFAULT '[]',
  affected_count    INT NOT NULL DEFAULT 0,
  fuel_delta_liters DOUBLE PRECISION NOT NULL DEFAULT 0,
  initiated_by      TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 家长通知（演示环境落库即视为已发送）
CREATE TABLE IF NOT EXISTS notifications (
  id         BIGSERIAL PRIMARY KEY,
  trip_id    BIGINT,
  student_id BIGINT,
  phone      TEXT NOT NULL,
  ntype      TEXT NOT NULL,
  content    TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'sent',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notifications_phone ON notifications(phone, id);

-- 异常与降级处置轨迹
CREATE TABLE IF NOT EXISTS exceptions (
  id          BIGSERIAL PRIMARY KEY,
  trip_id     BIGINT NOT NULL REFERENCES trips(id),
  etype       TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'open',
  student_id  BIGINT,
  reported_by TEXT NOT NULL DEFAULT '',
  resolution  TEXT NOT NULL DEFAULT '',
  resolved_by TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at TIMESTAMPTZ
);

-- 班主任确认学生状态
CREATE TABLE IF NOT EXISTS homeroom_confirmations (
  id           BIGSERIAL PRIMARY KEY,
  trip_id      BIGINT NOT NULL REFERENCES trips(id),
  student_id   BIGINT NOT NULL,
  teacher_name TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL, -- safe_at_school | safe_at_home | exception
  note         TEXT NOT NULL DEFAULT '',
  confirmed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(trip_id, student_id)
);

-- 车辆油耗记录
CREATE TABLE IF NOT EXISTS fuel_logs (
  id           BIGSERIAL PRIMARY KEY,
  trip_id      BIGINT NOT NULL REFERENCES trips(id),
  vehicle_id   BIGINT NOT NULL,
  delta_liters DOUBLE PRECISION NOT NULL,
  reason       TEXT NOT NULL DEFAULT '',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
