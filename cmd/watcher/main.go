package main

import (
	"flag"
	"log"
	"os"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to watcher config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("loaded %d watch mapping(s) from %s", len(cfg.Watches), *configPath)

	if os.Getenv("HATCHET_CLIENT_TOKEN") == "" {
		log.Fatal("HATCHET_CLIENT_TOKEN is not set")
	}

	_, err = hatchet.NewClient()
	if err != nil {
		log.Fatalf("failed to connect to Hatchet: %v", err)
	}

	log.Println("connected to Hatchet")

	// TODO(#7): start fsnotify directory watching
	select {}
}
