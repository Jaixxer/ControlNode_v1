package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	version := "1.0.0"
	var rootCmd = &cobra.Command{
		Use:   "ctl",
		Short: "This project is yet to be given a proper description",
	}
	var versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Prints the current version of controlPlane",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("version: " + version)
		},
	}
	var runCmd = &cobra.Command{
		Use:   "run",
		Short: "Used to run the controlPlane daemon with the config provided",
		Run: func(cmd *cobra.Command, args []string) {
			configPath, err := cmd.Flags().GetString("config")
			if err != nil {
				slog.Error("Failed to get config path", "error", err)
				os.Exit(1)
			}
			slog.Info("Config path", "path", configPath)
			params := url.Values{}
			params.Set("config", configPath)
			urlAdd := GetUrl("/run", params)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlAdd.String(), nil)
			if err != nil {
				slog.Error("Error creating the request: ", "error", err)
				os.Exit(1)
			}
			client := DaemonClient()
			resp, err := client.Do(req)
			if err != nil {
				slog.Error("Error running the daemon: ", "error", err)
				os.Exit(1)
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				fmt.Println(scanner.Text())
			}

		},
	}
	runCmd.Flags().String("config", "none", "Config file path")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(runCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Println("Error Occured: ", err)
		os.Exit(1)
	}
}
func GetUrl(path string, params url.Values) *url.URL {
	urlAdd := url.URL{Scheme: "http", Host: "local", Path: path, RawQuery: params.Encode()}
	return &urlAdd
}
func DaemonClient() *http.Client {
	dialer := &net.Dialer{}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", "/tmp/cplane.sock")

		},
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return client

}
