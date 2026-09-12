// cli 是一个极简 gRPC 客户端，用于在容器内直接验证 gRPC 接口。
// 用法：cli -addr localhost:9090 <ping|trip <id>|trace <id>|fulfillment <route_id> [date]>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "schoolbus/gen/schoolbusv1"
)

func main() {
	addr := flag.String("addr", envOr("GRPC_ADDR", "localhost:9090"), "gRPC 地址")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: cli [-addr host:port] <ping|trip <id>|trace <id>|fulfillment <route_id> [date]>")
		os.Exit(2)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接失败:", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := protojson.MarshalOptions{EmitUnpopulated: true, Indent: "  "}

	print := func(m proto.Message) {
		b, err := out.Marshal(m)
		if err != nil {
			fmt.Fprintln(os.Stderr, "编码失败:", err)
			os.Exit(1)
		}
		fmt.Println(string(b))
	}

	switch args[0] {
	case "ping":
		resp, err := pb.NewAdminServiceClient(conn).Ping(ctx, &pb.PingRequest{})
		must(err)
		print(resp)
	case "trip":
		id := mustID(args, 1)
		resp, err := pb.NewTripServiceClient(conn).GetTrip(ctx, &pb.GetTripRequest{TripId: id})
		must(err)
		print(resp)
	case "trace":
		id := mustID(args, 1)
		resp, err := pb.NewTripServiceClient(conn).GetTripTrace(ctx, &pb.GetTripTraceRequest{TripId: id})
		must(err)
		print(resp)
	case "fulfillment":
		routeID := mustID(args, 1)
		date := ""
		if len(args) > 2 {
			date = args[2]
		}
		resp, err := pb.NewAuditServiceClient(conn).GetRouteFulfillment(ctx, &pb.RouteFulfillmentRequest{RouteId: routeID, Date: date})
		must(err)
		print(resp)
	default:
		fmt.Fprintln(os.Stderr, "未知命令:", args[0])
		os.Exit(2)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "调用失败:", err)
		os.Exit(1)
	}
}

func mustID(args []string, i int) int64 {
	if len(args) <= i {
		fmt.Fprintln(os.Stderr, "缺少 id 参数")
		os.Exit(2)
	}
	id, err := strconv.ParseInt(args[i], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "id 格式错误:", err)
		os.Exit(2)
	}
	return id
}
