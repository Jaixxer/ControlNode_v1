package main

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"server"`
	Database struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	} `yaml:"database"`
}

func ParseConfig(path string) (*Config, error) {
	var config Config
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		slog.Error("Failed to read config file: ", "error", err)
		return nil, err
	}
	if err = yaml.Unmarshal(fileBytes, &config); err != nil {
		slog.Error("Error in config file/File not found: ", "error", err)
		return nil, err
	}
	return &config, nil
}
