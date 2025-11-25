package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go-enterprise-scheduler/pkg/models"
)

const targetURL = "http://localhost:8080/api/v1/dag"

type LResult struct {
	Latency    time.Duration
	StatusCode int
}

func main() {
	totalRequests := 100
	concurrency := 10
	dagSize := 5

	fmt.Printf("Config -> requests=%d concurrency=%d dagSize=%d\n", totalRequests, concurrency, dagSize)

	var successCount atomic.Int32
	var wg sync.WaitGroup
	results := make([]LResult, totalRequests)

	reqChan := make(chan int, totalRequests)
	for i := 0; i < totalRequests; i++ {
		reqChan <- i
	}
	close(reqChan)

	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for reqID := range reqChan {
				tasks := generateDAG(reqID, dagSize)
				data, _ := json.Marshal(tasks)
				req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(data))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", fmt.Sprintf("lt-%d-%d", time.Now().UnixNano(), reqID))

				begin := time.Now()
				resp, err := client.Do(req)
				dur := time.Since(begin)

				if err != nil {
					results[reqID] = LResult{Latency: dur, StatusCode: 0}
				} else {
					results[reqID] = LResult{Latency: dur, StatusCode: resp.StatusCode}
					resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						successCount.Add(1)
					}
				}
				fmt.Printf("request %d done (status: %d)\n", reqID, results[reqID].StatusCode)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("\nSuccess: %d/%d\n", successCount.Load(), totalRequests)
}

func generateDAG(reqID int, dagSize int) []models.Task {
	var tasks []models.Task
	for i := 0; i < dagSize; i++ {
		payload := fmt.Sprintf("echo 'Load test %d task %d'", reqID, i)
		if rand.Float32() < 0.15 {
			payload = "fail"
		}
		task := models.Task{
			ID:           fmt.Sprintf("task-%d-%d", reqID, i),
			Payload:      payload,
			Priority:     rand.Intn(10),
			Dependencies: []string{},
		}
		if i > 0 && rand.Float32() > 0.5 {
			task.Dependencies = append(task.Dependencies, fmt.Sprintf("task-%d-%d", reqID, i-1))
		}
		tasks = append(tasks, task)
	}
	return tasks
}
