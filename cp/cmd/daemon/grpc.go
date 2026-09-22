package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	pb "jaiveer/ControlPlane/pkg/proto"
	"log/slog"
	"net"
	"os"
	"path"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type GrpcServer struct {
	pb.UnimplementedEchoServiceServer
	pb.UnimplementedRegisterWorkerServer
}

func (s *GrpcServer) Echo(ctx context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	return &pb.EchoResponse{Message: "Wassup" + req.Message}, nil
}

// Register is a demo bidirectional stream: it keeps reading worker requests
// and replies to each one with a task until the client closes the stream.
func (s *GrpcServer) Register(stream grpc.BidiStreamingServer[pb.RegisterWorkerRequest, pb.RegisterWorkerResponse]) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			slog.Info("Worker closed the register stream")
			return nil
		}
		if err != nil {
			return err
		}
		slog.Info("Received register request", "name", req.Name, "heartbeat", req.Heartbeat)
		resp := &pb.RegisterWorkerResponse{
			Task:    fmt.Sprintf("Hello %s, you are registered with the control plane (heartbeat %d)", req.Name, req.Heartbeat),
			Success: true,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}
func (s *GrpcServer) InitGrpcServer(config *Config) error {
	serverCert, err := tls.LoadX509KeyPair(path.Join(config.Pki.PkiRootPath, config.Pki.ServerCertPath), path.Join(config.Pki.PkiRootPath, config.Pki.ServerKeyPath))
	if err != nil {
		return err
	}
	caCert, err := os.ReadFile(path.Join(config.Pki.PkiRootPath, config.Pki.CaCertPath))
	if err != nil {
		return err
	}
	//CertPool is created in order to isolate the CA cert from rest of the system CA certs
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(caCert) {
		return errors.New("Unable to parse PEM CA cert from file")
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    certPool,
	}
	creds := credentials.NewTLS(tlsConfig)
	opts := []grpc.ServerOption{
		grpc.Creds(creds),
	}
	server := grpc.NewServer(opts...)
	grpcServer := &GrpcServer{}
	pb.RegisterEchoServiceServer(server, grpcServer)
	pb.RegisterRegisterWorkerServer(server, grpcServer)
	lis, err := net.Listen("tcp", "localhost:50051")
	if err != nil {
		return err
	}
	if err := server.Serve(lis); err != nil {
		return err
	}
	return nil
}
