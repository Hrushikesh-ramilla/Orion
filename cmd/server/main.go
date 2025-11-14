package main

import (
	"fmt"
	"log"
	"os"

	"go-enterprise-scheduler/internal/storage"
)

func main() {
	fmt.Println("=== Distributed Task Orchestrator ===")
	fmt.Println("Starting up...")

	walPath := os.Getenv("WAL_PATH")
	if walPath == "" {
		walPath = "wal.json"
	}
	wal, err := storage.NewWAL(walPath)
	if err != nil {
		log.Fatalf("failed to initialize WAL: %v", err)
	}
	defer wal.Close()
	fmt.Printf("WAL initialized at %s\n", walPath)

	fmt.Println("Server ready. (no scheduler yet)")
}
