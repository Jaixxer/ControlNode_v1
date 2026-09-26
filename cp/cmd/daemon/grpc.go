package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	pb "jaiveer/ControlPlane/pkg/proto"
	"net"
	"os"
	"path"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type GrpcServer struct {
	pb.UnimplementedEchoServiceServer
	pb.UnimplementedRegisterWorkerServer
	// Workers tracks the workers attached over the Register stream so deploys
	// can be pushed to them.
	Workers *workerRegistry
	// Deploys records the status workers report back for each deploy.
	Deploys *deployTracker
	// Store persists worker liveness (last seen / online status).
	Store *workerStore
}

func (s *GrpcServer) Echo(ctx context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	return &pb.EchoResponse{Message: "Wassup" + req.Message}, nil
}

// Register keeps a worker attached to the control plane. The worker's name comes
// from its first message; after that the stream is used to push deploy commands
// down to it and to observe heartbeats.
func (s *GrpcServer) Register(stream grpc.BidiStreamingServer[pb.RegisterWorkerRequest, pb.RegisterWorkerResponse]) error {
	if s.Workers == nil {
		return errors.New("worker registry is not initialised")
	}
	return s.registerStream(stream)
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
	pb.RegisterEchoServiceServer(server, s)
	pb.RegisterRegisterWorkerServer(server, s)
	lis, err := net.Listen("tcp", "localhost:50051")
	if err != nil {
		return err
	}
	if err := server.Serve(lis); err != nil {
		return err
	}
	return nil
}
