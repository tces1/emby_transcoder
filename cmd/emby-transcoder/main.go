package main

import (
	"flag"
	"log"
	"net/http"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/proxy"
)

func main() {
	configPath := flag.String("config", "", "path to config JSON file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	handler, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("create proxy: %v", err)
	}

	log.Printf("EmbyTranscoder listening on %s, upstream %s", cfg.Server.Listen, cfg.Upstream.URL)
	if err := http.ListenAndServe(cfg.Server.Listen, handler); err != nil {
		log.Fatal(err)
	}
}
