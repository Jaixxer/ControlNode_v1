package main

import (
	"context"
	"fmt"
	database "jaiveer/ControlPlane/cp/internal/db"
	"log/slog"
	"net"
	"net/http"
	"os"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	_ = os.RemoveAll("/tmp/cplane.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("/run", run)
	listener, err := net.Listen("unix", "/tmp/cplane.sock")
	os.Chmod("/tmp/cplane.sock", 0700)
	if err != nil {
		slog.Error("failed to listen", "error", err)
		os.Exit(1)
	}
	slog.Info("listening", "address", listener.Addr())
	http.Serve(listener, mux)
}

func run(w http.ResponseWriter, r *http.Request) {
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
	user, err := gorm.G[database.Workers](db).Find(context.Background())
	if err != nil {
		fmt.Fprintln(w, "Error finding the worker: ", err)
		flusher.Flush()
	}
	fmt.Fprintf(w, "%+v\n", user)
	flusher.Flush()
}
