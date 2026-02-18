package main

import (
	"flag"
	"fmt"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server ServerConfig
	Config ConfigSection
}

type ServerConfig struct {
	Address string `toml:"address"`
	Port    int    `toml:"port"`
}

type ConfigSection struct {
	HMACKey string `toml:"hmacKey"`
}

func loadConfig() (*Config, error) {
	path := flag.String("config", "nitter-proxy.conf", "path to config file")
	flag.Parse()

	cfg := &Config{
		Server: ServerConfig{
			Address: "localhost",
			Port:    7000,
		},
		Config: ConfigSection{
			HMACKey: "secretkey",
		},
	}

	if _, err := toml.DecodeFile(*path, cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", *path, err)
	}

	return cfg, nil
}
