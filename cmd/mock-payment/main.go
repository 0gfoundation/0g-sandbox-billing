// mock-payment is a minimal Payment Layer stub for local development and testing.
//
// Endpoints:
//
//	POST /deposit                          accept a deposit request → returns {"request_id":"..."}
//	GET  /deposit/status?id=<request_id>   poll status → {"status":"pending|success|failed"}
//	POST /admin/resolve?id=<id>&status=<s> manually set status to "success" or "failed"
//	GET  /admin/list                        list all pending requests
//	GET  /health                            liveness probe
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type depositRecord struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Provider  string    `json:"provider"`
	Amount    string    `json:"amount"`
	Status    string    `json:"status"` // pending | success | failed
	CreatedAt time.Time `json:"created_at"`
}

var (
	mu       sync.RWMutex
	requests = map[string]*depositRecord{}
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})

	mux.HandleFunc("/deposit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			User      string `json:"user"`
			Provider  string `json:"provider"`
			Amount    string `json:"amount"`
			Timestamp int64  `json:"timestamp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id := uuid.New().String()
		rec := &depositRecord{
			ID:        id,
			User:      body.User,
			Provider:  body.Provider,
			Amount:    body.Amount,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		mu.Lock()
		requests[id] = rec
		mu.Unlock()

		log.Printf("deposit request created: id=%s user=%s provider=%s amount=%s", id, body.User, body.Provider, body.Amount)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"request_id": id}) //nolint:errcheck
	})

	mux.HandleFunc("/deposit/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		mu.RLock()
		rec, ok := requests[id]
		mu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": rec.Status}) //nolint:errcheck
	})

	mux.HandleFunc("/admin/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		status := r.URL.Query().Get("status")
		if status != "success" && status != "failed" {
			http.Error(w, `status must be "success" or "failed"`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		rec, ok := requests[id]
		if ok {
			rec.Status = status
		}
		mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("deposit request resolved: id=%s status=%s", id, status)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id, "status": status}) //nolint:errcheck
	})

	mux.HandleFunc("/admin/list", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		list := make([]*depositRecord, 0, len(requests))
		for _, rec := range requests {
			list = append(list, rec)
		}
		mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list) //nolint:errcheck
	})

	addr := ":9090"
	log.Printf("mock payment layer listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
