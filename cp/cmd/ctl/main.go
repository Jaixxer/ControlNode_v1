package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

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
			}
			slog.Info("Config path", "path", configPath)
			urlAdd := fmt.Sprintf("http://local/run?config=%s", configPath)
			client := DaemonClient()
			resp, err := client.Get(urlAdd)
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

func DaemonClient() *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("unix", "/tmp/cplane.sock")
		},
	}
	client := &http.Client{Transport: transport}
	return client

}
