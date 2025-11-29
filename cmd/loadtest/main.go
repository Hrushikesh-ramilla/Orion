package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go-enterprise-scheduler/pkg/models"
)

const (
	targetURL = "http://localhost:8080/api/v1/dag"
)

type Result struct {
	Latency    time.Duration
	StatusCode int
}

func main() {
	var totalRequests = flag.Int("requests", 1000, "total number of requests")
	var concurrency = flag.Int("concurrency", 50, "number of concurrent workers")
	var dagSize = flag.Int("dag", 10, "tasks per DAG")

	flag.Parse()

	baseReqs := *totalRequests
	baseConc := *concurrency
	baseDag := *dagSize

	fmt.Printf("Config -> requests=%d concurrency=%d dagSize=%d\n", *totalRequests, *concurrency, *dagSize)

	fmt.Println("====================================")
	runScenario(baseReqs, max(1, baseConc/5), baseDag)   // baseline
	fmt.Println("====================================")
	runScenario(baseReqs, baseConc, baseDag)             // medium
	fmt.Println("====================================")
	runScenario(baseReqs, baseConc*4, baseDag)           // stress
	fmt.Println("====================================")
}

func max(a, b int) int {
	if a > b { return a }
	return b
}

func runScenario(totalRequests, concurrency, dagSize int) {
	var (
		successCount    atomic.Int32
		rateLimitCount  atomic.Int32
		serverErrCount  atomic.Int32
		networkErrCount atomic.Int32
		results         = make([]Result, totalRequests)
		wg              sync.WaitGroup
		startTime       = time.Now()
	)

	reqChan := make(chan int, totalRequests)
	for i := 0; i < totalRequests; i++ {
		reqChan <- i
	}
	close(reqChan)

	client := &http.Client{Timeout: 5 * time.Second}

	fmt.Printf("\n[SCENARIO] requests=%d concurrency=%d dag=%d\n", totalRequests, concurrency, dagSize)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for reqID := range reqChan {
				res := executeRequest(client, reqID, dagSize)
				results[reqID] = res
				
				if res.StatusCode == 0 {
					networkErrCount.Add(1)
				} else if res.StatusCode >= 200 && res.StatusCode < 300 {
					successCount.Add(1)
				} else if res.StatusCode == http.StatusTooManyRequests {
					rateLimitCount.Add(1)
				} else if res.StatusCode >= 500 {
					serverErrCount.Add(1)
				}
				fmt.Printf("request completed %d (status: %d)\n", reqID, res.StatusCode)
				fmt.Println("request completed", reqID)
			}
		}()
	}

	wg.Wait()
	duration := time.Since(startTime)

	var latencies []time.Duration
	for _, r := range results {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			latencies = append(latencies, r.Latency)
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	var totalLatency time.Duration
	for _, l := range latencies {
		totalLatency += l
	}

	avgLatency := time.Duration(0)
	if len(latencies) > 0 {
		avgLatency = totalLatency / time.Duration(len(latencies))
	}

	p50 := time.Duration(0)
	p95 := time.Duration(0)
	p99 := time.Duration(0)

	if len(latencies) > 0 {
		p50 = latencies[int(float64(len(latencies))*0.50)]
		p95 = latencies[int(float64(len(latencies))*0.95)]
		p99 = latencies[int(float64(len(latencies))*0.99)]
	}
	
	var under50, under100, under200, under500, over500 int
	for _, l := range latencies {
		switch {
		case l < 50*time.Millisecond:
			under50++
		case l < 100*time.Millisecond:
			under100++
		case l < 200*time.Millisecond:
			under200++
		case l < 500*time.Millisecond:
			under500++
		default:
			over500++
		}
	}

	rps := float64(totalRequests) / duration.Seconds()

	fmt.Println("\n---")
	successPct := float64(successCount.Load()) / float64(totalRequests) * 100.0
	fmt.Printf("Total Requests:       %d\n", totalRequests)
	fmt.Printf("Success %%:            %.2f%%\n", successPct)
	fmt.Printf("Success Count:        %d\n", successCount.Load())
	fmt.Printf("Rate Limited (429):   %d\n", rateLimitCount.Load())
	fmt.Printf("Server Errors:        %d\n", serverErrCount.Load())
	fmt.Printf("Network Errors:       %d\n", networkErrCount.Load())
	fmt.Printf("Throughput (req/sec): %.2f\n", rps)
	fmt.Printf("Avg Latency:          %v\n", avgLatency)
	fmt.Printf("P50 / P95 / P99:      %v / %v / %v\n", p50, p95, p99)
	fmt.Println("Latency Buckets:")
	fmt.Printf("  <50ms:    %d\n", under50)
	fmt.Printf("  <100ms:   %d\n", under100)
	fmt.Printf("  <200ms:   %d\n", under200)
	fmt.Printf("  <500ms:   %d\n", under500)
	fmt.Printf("  >=500ms:  %d\n", over500)

	// Wait for the DAG engine to fully drain to verify true execution states!
	fmt.Println("\nWaiting for execution engine to drain internal queues...")
	var st map[string]int
	for {
		resp, err := client.Get("http://localhost:8080/api/v1/status")
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			if st["pending"] == 0 && st["running"] == 0 && (st["completed"] > 0 || st["failed"] > 0) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	execTotal := st["completed"] + st["failed"]
	execPct := 0.0
	if execTotal > 0 {
		execPct = float64(st["completed"]) / float64(execTotal) * 100.0
	}
	fmt.Println("\n--- ENGINE EXECUTION STATS ---")
	fmt.Printf("Tasks Tracked:        %d\n", execTotal)
	fmt.Printf("Tasks Executed:       %d\n", st["completed"])
	fmt.Printf("Tasks Failed:         %d\n", st["failed"])
	fmt.Printf("Tasks Retried:        %d\n", st["retried"])
	fmt.Printf("Engine Success Rate:  %.2f%%\n", execPct)
	fmt.Println("---")
}

func executeRequest(client *http.Client, reqID int, dagSize int) Result {
	begin := time.Now()
	
	tasks := generateDAG(reqID, dagSize)
	data, _ := json.Marshal(tasks)

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(data))
	if err != nil {
		return Result{Latency: time.Since(begin), StatusCode: 0}
	}
	
	req.Header.Set("Content-Type", "application/json")
	idempotencyKey := fmt.Sprintf("loadtest-%d-%d", time.Now().UnixNano(), reqID)
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := client.Do(req)
	duration := time.Since(begin)

	if err != nil {
		return Result{Latency: duration, StatusCode: 0}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return Result{Latency: duration, StatusCode: resp.StatusCode}
}

func generateDAG(reqID int, dagSize int) []models.Task {
	var tasks []models.Task
	
	for i := 0; i < dagSize; i++ {
		payload := fmt.Sprintf("echo 'Load test %d task %d'", reqID, i)
		if rand.Float32() < 0.15 { // 15% physical execution failure injection
			payload = "fail"
		}

		task := models.Task{
			ID:           fmt.Sprintf("task-%d-%d", reqID, i),
			Payload:      payload,
			Priority:     rand.Intn(10), // Random Priority
			Dependencies: []string{},
		}
		
		// Random chain dependency (50% probability to attach to preceding node)
		if i > 0 && rand.Float32() > 0.5 {
			task.Dependencies = append(task.Dependencies, fmt.Sprintf("task-%d-%d", reqID, i-1))
		}
		
		// Additional fan-out behavior (20% probability to attach to an older, distinct node)
		if i > 1 && rand.Float32() > 0.8 {
			olderIdx := rand.Intn(i - 1)
			task.Dependencies = append(task.Dependencies, fmt.Sprintf("task-%d-%d", reqID, olderIdx))
		}
		
		tasks = append(tasks, task)
	}
	
	return tasks
}
