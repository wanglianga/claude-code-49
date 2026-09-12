// Package httpapi 提供与 gRPC 等价的 JSON/REST 门面（protojson 编解码），
// 便于浏览器与 curl 走通关键业务流；核心逻辑全部在 service 层。
package httpapi

import (
	"context"
	_ "embed"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "schoolbus/gen/schoolbusv1"
	"schoolbus/internal/service"
)

//go:embed static/index.html
var indexHTML []byte

var marshalOpts = protojson.MarshalOptions{EmitUnpopulated: true, UseProtoNames: true}
var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

// NewHandler 注册全部 HTTP 路由。
func NewHandler(svc *service.Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	// 总览
	get(mux, "/api/overview", func() *pb.GetOverviewRequest { return &pb.GetOverviewRequest{} }, svc.GetOverview)

	// 基础数据维护
	post(mux, "/api/admin/schools", func() *pb.School { return &pb.School{} }, svc.CreateSchool)
	post(mux, "/api/admin/classes", func() *pb.Class { return &pb.Class{} }, svc.CreateClass)
	post(mux, "/api/admin/students", func() *pb.Student { return &pb.Student{} }, svc.CreateStudent)
	post(mux, "/api/admin/guardians", func() *pb.Guardian { return &pb.Guardian{} }, svc.CreateGuardian)
	post(mux, "/api/admin/authorizations", func() *pb.PickupAuthorization { return &pb.PickupAuthorization{} }, svc.CreatePickupAuthorization)
	post(mux, "/api/admin/stops", func() *pb.Stop { return &pb.Stop{} }, svc.CreateStop)
	post(mux, "/api/admin/time-windows", func() *pb.TimeWindow { return &pb.TimeWindow{} }, svc.CreateTimeWindow)
	post(mux, "/api/admin/vehicles", func() *pb.Vehicle { return &pb.Vehicle{} }, svc.CreateVehicle)
	post(mux, "/api/admin/drivers", func() *pb.Driver { return &pb.Driver{} }, svc.CreateDriver)
	post(mux, "/api/admin/escorts", func() *pb.Escort { return &pb.Escort{} }, svc.CreateEscort)
	post(mux, "/api/admin/routes", func() *pb.CreateRouteRequest { return &pb.CreateRouteRequest{} }, svc.CreateRoute)
	post(mux, "/api/admin/assignments", func() *pb.StudentRouteAssignment { return &pb.StudentRouteAssignment{} }, svc.AssignStudentRoute)
	post(mux, "/api/admin/schedules", func() *pb.Schedule { return &pb.Schedule{} }, svc.CreateSchedule)

	// 接送任务
	post(mux, "/api/trips/generate", func() *pb.GenerateTripsRequest { return &pb.GenerateTripsRequest{} }, svc.GenerateTrips)
	mux.HandleFunc("GET /api/trips", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.ListTripsRequest{Date: r.URL.Query().Get("date")}
		req.RouteId, _ = strconv.ParseInt(r.URL.Query().Get("route_id"), 10, 64)
		resp, err := svc.ListTrips(r.Context(), req)
		writeResp(w, resp, err)
	})
	mux.HandleFunc("GET /api/trips/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		resp, err := svc.GetTrip(r.Context(), &pb.GetTripRequest{TripId: id})
		writeResp(w, resp, err)
	})
	postPath(mux, "/api/trips/{id}/confirm-vehicle", "trip_id", func() *pb.ConfirmVehicleRequest { return &pb.ConfirmVehicleRequest{} }, svc.ConfirmVehicle)
	postPath(mux, "/api/trips/{id}/confirm-roster", "trip_id", func() *pb.ConfirmRosterRequest { return &pb.ConfirmRosterRequest{} }, svc.ConfirmRoster)
	postPath(mux, "/api/trips/{id}/start", "trip_id", func() *pb.StartTripRequest { return &pb.StartTripRequest{} }, svc.StartTrip)
	postPath(mux, "/api/trips/{id}/arrive-stop", "trip_id", func() *pb.ArriveStopRequest { return &pb.ArriveStopRequest{} }, svc.ArriveStop)
	postPath(mux, "/api/trips/{id}/board", "trip_id", func() *pb.BoardRequest { return &pb.BoardRequest{} }, svc.BoardStudent)
	postPath(mux, "/api/trips/{id}/alight", "trip_id", func() *pb.AlightRequest { return &pb.AlightRequest{} }, svc.AlightStudent)
	postPath(mux, "/api/trips/{id}/absent", "trip_id", func() *pb.MarkAbsentRequest { return &pb.MarkAbsentRequest{} }, svc.MarkAbsent)
	postPath(mux, "/api/trips/{id}/contact", "trip_id", func() *pb.RecordContactRequest { return &pb.RecordContactRequest{} }, svc.RecordContact)
	post(mux, "/api/wait-records/update", func() *pb.UpdateWaitRecordRequest { return &pb.UpdateWaitRecordRequest{} }, svc.UpdateWaitRecord)
	postPath(mux, "/api/trips/{id}/complete", "trip_id", func() *pb.CompleteTripRequest { return &pb.CompleteTripRequest{} }, svc.CompleteTrip)
	postPath(mux, "/api/trips/{id}/homeroom-confirm", "trip_id", func() *pb.HomeroomConfirmRequest { return &pb.HomeroomConfirmRequest{} }, svc.HomeroomConfirm)
	mux.HandleFunc("GET /api/trips/{id}/trace", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		resp, err := svc.GetTripTrace(r.Context(), &pb.GetTripTraceRequest{TripId: id})
		writeResp(w, resp, err)
	})
	postPath(mux, "/api/trips/{id}/exceptions", "trip_id", func() *pb.ReportExceptionRequest { return &pb.ReportExceptionRequest{} }, svc.ReportException)
	postPath(mux, "/api/exceptions/{id}/resolve", "exception_id", func() *pb.ResolveExceptionRequest { return &pb.ResolveExceptionRequest{} }, svc.ResolveException)
	postPath(mux, "/api/trips/{id}/sync-offline", "trip_id", func() *pb.SyncOfflineEventsRequest { return &pb.SyncOfflineEventsRequest{} }, svc.SyncOfflineEvents)
	postPath(mux, "/api/trips/{id}/sibling-ride", "trip_id", func() *pb.SiblingRideRequest { return &pb.SiblingRideRequest{} }, svc.RecordSiblingRide)
	post(mux, "/api/students/leave", func() *pb.ReportLeaveRequest { return &pb.ReportLeaveRequest{} }, svc.ReportLeave)

	// 改线
	postPath(mux, "/api/trips/{id}/reroute", "trip_id", func() *pb.InitiateRerouteRequest { return &pb.InitiateRerouteRequest{} }, svc.InitiateReroute)
	mux.HandleFunc("GET /api/trips/{id}/reroutes", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		resp, err := svc.ListReroutes(r.Context(), &pb.ListReroutesRequest{TripId: id})
		writeResp(w, resp, err)
	})

	// 家长端
	mux.HandleFunc("GET /api/students/{id}/trip-status", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		req := &pb.StudentTripStatusRequest{StudentId: id, Date: r.URL.Query().Get("date")}
		if d := r.URL.Query().Get("direction"); d != "" {
			if v, ok := pb.Direction_value[d]; ok {
				req.Direction = pb.Direction(v)
			}
		}
		resp, err := svc.GetStudentTripStatus(r.Context(), req)
		writeResp(w, resp, err)
	})
	mux.HandleFunc("GET /api/notifications", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.ListNotificationsRequest{Phone: r.URL.Query().Get("phone"), Date: r.URL.Query().Get("date")}
		resp, err := svc.ListNotifications(r.Context(), req)
		writeResp(w, resp, err)
	})
	postPath(mux, "/api/students/{id}/self-pickup", "student_id", func() *pb.SelfPickupRequest { return &pb.SelfPickupRequest{} }, svc.RequestSelfPickup)
	postPath(mux, "/api/students/{id}/guardian-swap", "student_id", func() *pb.GuardianSwapRequest { return &pb.GuardianSwapRequest{} }, svc.RequestGuardianSwap)
	postPath(mux, "/api/students/{id}/club", "student_id", func() *pb.ClubActivityRequest { return &pb.ClubActivityRequest{} }, svc.ReportClubActivity)

	// 审计 / 点名
	mux.HandleFunc("GET /api/audit/routes/{id}/fulfillment", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		req := &pb.RouteFulfillmentRequest{RouteId: id, Date: r.URL.Query().Get("date")}
		resp, err := svc.GetRouteFulfillment(r.Context(), req)
		writeResp(w, resp, err)
	})
	mux.HandleFunc("GET /api/trips/{id}/rollcall", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		resp, err := svc.GetTripRollcall(r.Context(), &pb.TripRollcallRequest{TripId: id})
		writeResp(w, resp, err)
	})

	return mux
}

// ---------------------------------------------------------------------------
// 路由辅助
// ---------------------------------------------------------------------------

func get[Req, Resp proto.Message](mux *http.ServeMux, path string, newReq func() Req, fn func(context.Context, Req) (Resp, error)) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		resp, err := fn(r.Context(), newReq())
		writeResp(w, resp, err)
	})
}

func post[Req, Resp proto.Message](mux *http.ServeMux, path string, newReq func() Req, fn func(context.Context, Req) (Resp, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeBody(w, r, newReq)
		if !ok {
			return
		}
		resp, err := fn(r.Context(), req)
		writeResp(w, resp, err)
	})
}

// postPath 注册带路径参数的 POST 路由，路径 id 通过 proto 反射写入指定字段。
func postPath[Req, Resp proto.Message](mux *http.ServeMux, path, idField string, newReq func() Req, fn func(context.Context, Req) (Resp, error)) {
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeBody(w, r, newReq)
		if !ok {
			return
		}
		if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil && id != 0 {
			setInt64Field(req, idField, id)
		}
		resp, err := fn(r.Context(), req)
		writeResp(w, resp, err)
	})
}

func decodeBody[Req proto.Message](w http.ResponseWriter, r *http.Request, newReq func() Req) (Req, bool) {
	req := newReq()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, status.Error(codes.InvalidArgument, "读取请求体失败"))
		return req, false
	}
	if len(body) > 0 {
		if err := unmarshalOpts.Unmarshal(body, req); err != nil {
			writeErr(w, status.Error(codes.InvalidArgument, "JSON 解析失败: "+err.Error()))
			return req, false
		}
	}
	return req, true
}

// setInt64Field 通过反射设置 proto 消息的 int64 字段（如 trip_id / student_id）。
func setInt64Field(m proto.Message, field string, v int64) {
	fd := m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd != nil && fd.Kind() == protoreflect.Int64Kind {
		m.ProtoReflect().Set(fd, protoreflect.ValueOfInt64(v))
	}
}

func writeResp(w http.ResponseWriter, resp proto.Message, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	out, err := marshalOpts.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out)
}

func writeErr(w http.ResponseWriter, err error) {
	code := status.Code(err)
	httpCode := http.StatusInternalServerError
	switch code {
	case codes.InvalidArgument:
		httpCode = http.StatusBadRequest
	case codes.NotFound:
		httpCode = http.StatusNotFound
	case codes.FailedPrecondition:
		httpCode = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpCode)
	msg := strings.ReplaceAll(err.Error(), `"`, `'`)
	_, _ = w.Write([]byte(`{"error":"` + msg + `","code":"` + code.String() + `"}`))
}
