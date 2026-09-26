package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	workers_pki "jaiveer/ControlPlane/workers/pki"
)

func main() {
	joinAddr := flag.String(
		"join-address",
		"",
		"Address of ControlPlane to join",
	)

	token := flag.String(
		"token",
		"",
		"Bootstrap token",
	)

	configPath := flag.String(
		"config",
		"/tmp/workers/",
		"Local path to store worker credentials",
	)

	flag.Parse()

	if *joinAddr == "" || *token == "" {
		flag.Usage()
		os.Exit(1)
	}

	certManager := &workers_pki.CertificateManager{}

	if err := certManager.LoadBootstrapToken(*token, *configPath); err != nil {
		slog.Error("Error loading bootstrap token", "error", err)
		os.Exit(1)
	}

	slog.Info("Worker credentls initialized")

	// The worker stays attached to the control plane until it is interrupted,
	// waiting for projects to deploy.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	grpcClient := GrpcClient{}
	if err := grpcClient.InitGrpcClient(ctx, *certManager, *configPath); err != nil {
		slog.Error("Worker stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("Worker shut down cleanly")
}
