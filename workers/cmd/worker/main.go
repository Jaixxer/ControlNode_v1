package main

import (
	"flag"
	workers_pki "jaiveer/ControlPlane/workers/pki"
	"log/slog"
	"os"
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

	grpcClient := GrpcClient{}
	err := grpcClient.InitGrpcClient(*certManager)
	if err != nil {
		slog.Error("Error connecting to Grpc server and calling the func: ", "error", err)
		os.Exit(1)
	}
	slog.Info("done?")
}
