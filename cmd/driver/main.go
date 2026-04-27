// Command driver starts the OpenShell compute driver for OpenShift. It listens
// on a Unix domain socket and serves the ComputeDriver gRPC service that the
// OpenShell gateway connects to.
package main

import (
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"github.com/zanetworker/openshell-driver-openshift/internal/driver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	socketPath := flag.String("socket", "/var/run/openshell-driver.sock",
		"Unix domain socket path for the gRPC server")

	cfg := driver.DefaultConfig()
	flag.StringVar(&cfg.Namespace, "namespace", cfg.Namespace,
		"Kubernetes namespace where sandboxes are provisioned")
	flag.StringVar(&cfg.SupervisorImage, "supervisor-image", cfg.SupervisorImage,
		"Container image that contains the supervisor binary")
	flag.StringVar(&cfg.SupervisorBinaryPath, "supervisor-binary-path", cfg.SupervisorBinaryPath,
		"Path to the supervisor binary inside the supervisor image")
	flag.StringVar(&cfg.SupervisorMountPath, "supervisor-mount-path", cfg.SupervisorMountPath,
		"Mount path for the supervisor binary volume in the agent container")
	flag.StringVar(&cfg.GatewayEndpoint, "gateway-endpoint", cfg.GatewayEndpoint,
		"Gateway gRPC endpoint for supervisor callback (OPENSHELL_ENDPOINT)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Clean up stale socket from a previous run.
	os.Remove(*socketPath)

	lis, err := net.Listen("unix", *socketPath)
	if err != nil {
		logger.Error("failed to listen", "socket", *socketPath, "error", err)
		os.Exit(1)
	}

	// Make socket accessible to other containers in the same pod.
	if err := os.Chmod(*socketPath, 0777); err != nil {
		logger.Warn("failed to chmod socket", "error", err)
	}

	d, err := driver.New(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize driver", "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	pb.RegisterComputeDriverServer(srv, d)
	reflection.Register(srv)

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		s := <-sig
		logger.Info("received signal, shutting down", "signal", s)
		srv.GracefulStop()
	}()

	logger.Info("driver listening", "socket", *socketPath, "namespace", cfg.Namespace)
	if err := srv.Serve(lis); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}
