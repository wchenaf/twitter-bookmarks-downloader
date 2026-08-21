package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rotisserie/eris"
)

// A Bookmarks GraphQL page of 20 tweets with their raw media JSON measures in
// the low hundreds of KB, so 16 MB clears any legitimate payload by two
// orders of magnitude while keeping an oversized POST from buffering without
// bound into memory.
const maxSyncPayloadBytes = 16 << 20

type SyncResponse struct {
	Success               bool   `json:"success"`
	Message               string `json:"message"`
	DuplicateLimitReached bool   `json:"duplicate_limit_reached"`
	SavedCount            int    `json:"saved_count"`
}

func StartServer(addr string) error {
	http.HandleFunc("/api/sync-raw", handleSyncRaw)

	PrintInfoF("Server starting on %s...", addr)
	return http.ListenAndServe(addr, nil)
}

func handleSyncRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	PrintInfoF("Receiving raw GraphQL response from client (%s)...", r.RemoteAddr)

	r.Body = http.MaxBytesReader(w, r.Body, maxSyncPayloadBytes)

	var fullResponse json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fullResponse); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			PrintError(eris.Wrap(err, "Raw sync payload exceeds size limit"))
			http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		PrintError(eris.Wrap(err, "Failed to decode raw sync payload"))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	response := ProcessSyncRaw(fullResponse)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
