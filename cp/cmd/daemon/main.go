package main

import (
	"fmt"
	database "jaiveer/ControlPlane/cp/internal/db"
	"jaiveer/ControlPlane/cp/internal/pki"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type ControlPlane struct {
	CertManager pki.CertificateManager
	Config      Config
	DB          gorm.DB
}

// NOTE: Rn, daemon isnt maintaining a state of config in sense its not storing where the config is and as a resutl every restart would mean providing config path again
func main() {
	_ = os.RemoveAll("/tmp/cplane.sock")
	controlPlane := ControlPlane{}
	mux := http.NewServeMux()
	mux.HandleFunc("/run", controlPlane.Run)
	mux.HandleFunc("/add-worker", controlPlane.addWorker)
	listener, err := net.Listen("unix", "/tmp/cplane.sock")
	os.Chmod("/tmp/cplane.sock", 0700)
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}
	slog.Info("listening", "address", listener.Addr())
	go func() {
		server := GrpcServer{}
		go func() {
			server.InitGrpcServer(&controlPlane.Config)
		}()
	}()
	http.Serve(listener, mux)
}

func (s *ControlPlane) Run(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Error("Flushing not supported")
		return
	}
	configPath := r.URL.Query().Get("config")
	if configPath == "" {
		fmt.Fprintln(w, "No config path was passed")
		return
	}
	fmt.Fprintln(w, "Reading the config from path: ", configPath)
	flusher.Flush()
	config, err := ParseConfig(configPath)
	s.Config = *config
	if err != nil {
		fmt.Fprintln(w, "Config Error: ", err)
		flusher.Flush()
		return
	}
	fmt.Fprintln(w, "Loading config: ", config)
	flusher.Flush()
	//Initialising db at the path
	db, err := gorm.Open(sqlite.Open(config.Database.Path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		fmt.Fprintln(w, "Error in connecting to the database: ", err)
	}
	flusher.Flush()
	s.DB = *db
	err = db.AutoMigrate(&database.Deployments{}, &database.Workers{})
	if err != nil {
		fmt.Fprintln(w, "Error Migrating the schema to database: ", err)
		flusher.Flush()
	}
	fmt.Fprintln(w, "Migration Successful!")
	// Testing by creating a random worker
	// err = gorm.G[database.Workers](db).Create(context.Background(), &database.Workers{})
	// if err != nil {
	// 	fmt.Fprintln(w, "Error Creating a test worker:", err)
	// 	flusher.Flush()
	// }
	// user, err := gorm.G[database.Workers](db).Find(context.Background())
	// if err != nil {
	// 	fmt.Fprintln(w, "Error finding the worker: ", err)
	// 	flusher.Flush()
	// }
	// fmt.Fprintf(w, "%+v\n", user)

	//Initializing Certificates
	err = s.CertManager.InitCACert(config.Pki.PkiRootPath, config.Pki.CaCertPath, config.Pki.CaKeyPath)
	if err != nil {
		fmt.Fprintln(w, "Error Initialising Ca Certs and Keys hehe", err)
		flusher.Flush()
	}
	err = s.CertManager.InitServerCert(config.Pki.PkiRootPath, config.Pki.ServerCertPath, config.Pki.ServerKeyPath)
	if err != nil {
		fmt.Fprintln(w, "Error Initialising Server Certs and Keys hehe", err)
		flusher.Flush()
	}
	go func() {
		server := GrpcServer{}
		if err := server.InitGrpcServer(&s.Config); err != nil {
			slog.Error("Error Initialising GRPC Server", "error", err)
			return
		}

	}()

	// //Intiializing GRPC Server
	// server := GrpcServer{}
	// go func() {
	// 	server.InitGrpcServer(config)
	// }()

}
func (s *ControlPlane) addWorker(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Fatal("Error initialising the flusher")
	}
	name := r.URL.Query().Get("name")
	token, err := s.CertManager.CreateWorkerKeyCert(name, "/home/jaiveer/")
	if err != nil {
		fmt.Fprintln(w, "Error Creating the Worker Certificate", err)
		flusher.Flush()
		return
	}
	//TODO:ALLOW USER TO ADD CUSTOM ADDRESS AND SUPPORT LOCAL AS WELL AS GLOBAL HOSTING AS WELL AS CONFIG PATH
	fmt.Fprintln(w, "Copy paste the following cmd\n ./worker --join-address localhost:50051 --token "+token+" --config /tmp/worker/")
	flusher.Flush()
}
