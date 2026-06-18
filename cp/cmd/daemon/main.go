package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
)

func main() {
	_ = os.RemoveAll("/tmp/cplane.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("/run", run)
	listener, err := net.Listen("unix", "/tmp/cplane.sock")
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
	fmt.Fprintln(w, "Reading the config from path: ", configPath)
	flusher.Flush()
	config, err := ParseConfig(configPath)
	if err != nil {
		fmt.Fprintln(w, "Config Error: ", err)
		flusher.Flush()
		return
	}
	fmt.Fprintln(w, "Loading config: ", config.Database.Password)
	flusher.Flush()
}
