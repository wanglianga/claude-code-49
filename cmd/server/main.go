// 县域校车临时改线与学生到站安全服务
// 单进程同时暴露 gRPC（默认 :9090）与 HTTP/JSON 门面（默认 :8080）。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	pb "schoolbus/gen/schoolbusv1"
	"schoolbus/internal/httpapi"
	"schoolbus/internal/seed"
	"schoolbus/internal/service"
	"schoolbus/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	logger := log.New(os.Stdout, "[schoolbus] ", log.LstdFlags)

	loc, err := time.LoadLocation(env("TZ", "Asia/Shanghai"))
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}

	db, err := store.Connect(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		logger.Fatalf("数据库连接失败: %v", err)
	}
	defer db.Close()
	logger.Print("数据库已连接并完成迁移")

	svc := service.New(db, loc)

	if env("SEED_DEMO", "false") == "true" {
		today := time.Now().In(loc).Format("2006-01-02")
		if err := seed.Run(context.Background(), svc, today); err != nil {
			logger.Fatalf("演示数据初始化失败: %v", err)
		}
	}

	// gRPC 服务
	grpcAddr := env("GRPC_ADDR", ":9090")
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		logger.Fatalf("gRPC 监听失败: %v", err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterAdminServiceServer(grpcServer, svc)
	pb.RegisterTripServiceServer(grpcServer, svc)
	pb.RegisterRerouteServiceServer(grpcServer, svc)
	pb.RegisterParentServiceServer(grpcServer, svc)
	pb.RegisterAuditServiceServer(grpcServer, svc)
	reflection.Register(grpcServer)

	// HTTP/JSON 门面
	httpAddr := env("HTTP_ADDR", ":8080")
	httpServer := &http.Server{
		Addr:              httpAddr,
		Handler:           httpapi.NewHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("gRPC 服务启动于 %s", grpcAddr)
		if err := grpcServer.Serve(lis); err != nil {
			logger.Fatalf("gRPC 服务异常: %v", err)
		}
	}()
	go func() {
		logger.Printf("HTTP/JSON 门面启动于 %s（演示台 http://localhost%s/）", httpAddr, httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP 服务异常: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Print("收到退出信号，正在优雅停机…")
	grpcServer.GracefulStop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
