package main

// POST /technician/beacons/delete   body: {"id": 3, "location_id": 1}
// Header: Authorization: Bearer <token>   (technicians only)
//
// Deletes one beacon from indoor_nav_locations.beacons.
// Who is allowed is decided by the existing /technician/access route, so this
// file uses exactly the same sign-in rules as the rest of the technician panel.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Asks this same server's /technician/access whether the token belongs to a technician.
func isTechnicianToken(ctx context.Context, authHeader string) bool {
	if authHeader == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/technician/access", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", authHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Authorized bool `json:"authorized"`
	}
	return json.NewDecoder(resp.Body).Decode(&body) == nil && body.Authorized
}

func deleteBeaconHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(status int, msg string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		if !isTechnicianToken(r.Context(), r.Header.Get("Authorization")) {
			fail(http.StatusForbidden, "Only technicians can delete beacons. Sign in again.")
			return
		}

		var in struct {
			ID         int64 `json:"id"`
			LocationID int64 `json:"location_id"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID <= 0 || in.LocationID <= 0 {
			fail(http.StatusBadRequest, "Missing beacon id or location id.")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		res, err := db.ExecContext(ctx,
			"DELETE FROM indoor_nav_locations.beacons WHERE beacon_id = ? AND location_id = ?",
			in.ID, in.LocationID)
		if err != nil {
			log.Printf("Delete beacon %d failed: %v", in.ID, err)
			fail(http.StatusInternalServerError, "Could not delete the beacon. Check the server terminal.")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(http.StatusNotFound, "This beacon no longer exists. Pull down to refresh.")
			return
		}
		log.Printf("Beacon %d deleted (location %d)", in.ID, in.LocationID)
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true, "id": in.ID})
	}
}
