package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	pb "jaiveer/ControlPlane/pkg/proto"
	workers_pki "jaiveer/ControlPlane/workers/pki"
	"log"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type GrpcClient struct {
}

func (s *GrpcClient) InitGrpcClient(certManager workers_pki.CertificateManager) error {
	client, err := tls.LoadX509KeyPair("/tmp/worker/worker.crt", "/tmp/worker/worker.key")
	if err != nil {
		return err
	}
	caCert, err := os.ReadFile("/tmp/worker/ca.crt")
	if err != nil {
		return err
	}
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return errors.New("Unable to append the PEM encoded cert to the cert pool")
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{client},
		RootCAs:      caCertPool,
	}
	creds := credentials.NewTLS(tlsConfig)

	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(creds))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()
	echoClient := pb.NewEchoServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := echoClient.Echo(ctx, &pb.EchoRequest{Message: "mTlS workign?"})
	if err != nil {
		return err
	}
	slog.Info("Response", "message", resp.Message)

	// Demo bidirectional stream: send a few register requests and read a reply for each.
	registerClient := pb.NewRegisterWorkerClient(conn)
	stream, err := registerClient.Register(context.Background())
	if err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		if err := stream.Send(&pb.RegisterWorkerRequest{Name: "worker-demo", Heartbeat: int32(i)}); err != nil {
			return err
		}
		regResp, err := stream.Recv()
		if err != nil {
			return err
		}
		fmt.Println("Message from control plane:", regResp.Task)
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	return nil
}
