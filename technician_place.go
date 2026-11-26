package main

// Turn a beacon into a searchable place (shop, restaurant, gate, restroom…).
//
//   GET  /technician/beacons/place?beacon_id=3
//        -> {"linked":true,"kind":"store","type":"SHOP","name":"Pull&Bear","place_id":5}
//   POST /technician/beacons/place
//        body: {"beacon_id":3, "type":"SHOP", "name":"Pull&Bear"}
//          or: {"floor_id":1, "minor":3, "type":"WC", "name":"Restrooms"}   (right after a new install)
//          or: {"beacon_id":3, "remove":true}
//
// The place gets its own walking point at the beacon's x/y (node code PLACE_B<beacon id>),
// connected both ways to the nearest corridor point, so routes can reach it.
// Technicians only (same check as the delete route).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Types stored as shops/restaurants (stores table) - the rest are facilities
var placeStoreTypes = map[string]bool{"SHOP": true, "FOOD": true}
var placeFacilityTypes = map[string]bool{
	"ELEVATOR": true, "ESCALATOR": true, "STAIRS": true, "WC": true, "GATE": true, "ENTRANCE": true, "EXIT": true,
	"ATM": true, "INFORMATION": true, "SECURITY": true, "PARKING": true, "ROOM": true, "CHECK_IN": true,
	"BAGGAGE_CLAIM": true, "OTHER": true,
}

func placeNodeType(t string) string {
	switch t {
	case "GATE", "ENTRANCE", "EXIT", "ELEVATOR", "ESCALATOR", "STAIRS":
		return t
	}
	return "DESTINATION"
}

type beaconRow struct {
	id, location, floor int64
	x, y                float64
	node                sql.NullInt64 // walking point the beacon is already linked to, if any
}

func findBeacon(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, beaconID, floorID int64, minor int) (beaconRow, error) {
	var b beaconRow
	var row *sql.Row
	if beaconID > 0 {
		row = q.QueryRowContext(ctx, `SELECT beacon_id, location_id, floor_id, x_coordinate, y_coordinate, navigation_node_id
			FROM indoor_nav_locations.beacons WHERE beacon_id = ?`, beaconID)
	} else {
		row = q.QueryRowContext(ctx, `SELECT beacon_id, location_id, floor_id, x_coordinate, y_coordinate, navigation_node_id
			FROM indoor_nav_locations.beacons WHERE floor_id = ? AND beacon_minor = ? ORDER BY beacon_id DESC LIMIT 1`, floorID, minor)
	}
	err := row.Scan(&b.id, &b.location, &b.floor, &b.x, &b.y, &b.node)
	return b, err
}

func beaconPlaceHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(status int, msg string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
		}
		if !isTechnicianToken(r.Context(), r.Header.Get("Authorization")) {
			fail(http.StatusForbidden, "Only technicians can change places. Sign in again.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		// ── GET: what place is this beacon linked to? ──
		if r.Method == http.MethodGet {
			id, _ := strconv.ParseInt(r.URL.Query().Get("beacon_id"), 10, 64)
			b, err := findBeacon(ctx, db, id, 0, 0)
			if err != nil {
				fail(http.StatusNotFound, "Beacon not found.")
				return
			}
			code := fmt.Sprintf("PLACE_B%d", b.id)
			var kind, typ, name string
			var pid int64
			err = db.QueryRowContext(ctx, `
				SELECT 'store', UPPER(c.category_name), s.store_name, s.store_id
				  FROM indoor_nav_locations.stores s
				  JOIN indoor_nav_locations.store_categories c ON c.category_id = s.category_id
				  JOIN indoor_nav_locations.navigation_nodes n ON n.node_id = s.navigation_node_id
				 WHERE n.location_id = ? AND n.floor_id = ? AND (n.node_code = ? OR n.node_id = ?) AND s.is_active = 1
				UNION ALL
				SELECT 'facility', f.facility_type, f.facility_name, f.facility_id
				  FROM indoor_nav_locations.facilities f
				  JOIN indoor_nav_locations.navigation_nodes n ON n.node_id = f.navigation_node_id
				 WHERE n.location_id = ? AND n.floor_id = ? AND (n.node_code = ? OR n.node_id = ?) AND f.is_active = 1
				 LIMIT 1`, b.location, b.floor, code, b.node.Int64, b.location, b.floor, code, b.node.Int64).Scan(&kind, &typ, &name, &pid)
			if err == sql.ErrNoRows {
				_ = json.NewEncoder(w).Encode(map[string]any{"linked": false})
				return
			}
			if err != nil {
				log.Printf("beacon place read: %v", err)
				fail(http.StatusInternalServerError, "Could not read the place. Check the server terminal.")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"linked": true, "kind": kind, "type": typ, "name": name, "place_id": pid})
			return
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			fail(http.StatusMethodNotAllowed, "Method not allowed")
			return
		}

		var in struct {
			BeaconID int64  `json:"beacon_id"`
			FloorID  int64  `json:"floor_id"`
			Minor    int    `json:"minor"`
			Type     string `json:"type"`
			Name     string `json:"name"`
			Remove   bool   `json:"remove"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(http.StatusBadRequest, "Invalid request.")
			return
		}
		in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
		in.Name = strings.TrimSpace(in.Name)
		isStore := placeStoreTypes[in.Type]
		if !in.Remove {
			if !isStore && !placeFacilityTypes[in.Type] {
				fail(http.StatusBadRequest, "Unknown place type.")
				return
			}
			if in.Name == "" || len([]rune(in.Name)) > 150 {
				fail(http.StatusBadRequest, "Give the place a name (150 characters maximum).")
				return
			}
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fail(http.StatusInternalServerError, "Database error.")
			return
		}
		defer tx.Rollback()

		b, err := findBeacon(ctx, tx, in.BeaconID, in.FloorID, in.Minor)
		if err != nil {
			fail(http.StatusNotFound, "Beacon not found. Save the beacon first.")
			return
		}
		code := fmt.Sprintf("PLACE_B%d", b.id)

		// the place's walking point
		var node int64
		err = tx.QueryRowContext(ctx, `SELECT node_id FROM indoor_nav_locations.navigation_nodes
			WHERE location_id = ? AND floor_id = ? AND node_code = ?`, b.location, b.floor, code).Scan(&node)
		if err != nil && err != sql.ErrNoRows {
			fail(http.StatusInternalServerError, "Database error.")
			return
		}
		if node == 0 && b.node.Valid {
			var used int
			_ = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM indoor_nav_locations.stores WHERE navigation_node_id = ?) +
				(SELECT COUNT(*) FROM indoor_nav_locations.facilities WHERE navigation_node_id = ?)`, b.node.Int64, b.node.Int64).Scan(&used)
			if used > 0 {
				node = b.node.Int64 // the beacon already marks a place: edit that one instead of adding a second
			}
		}

		// existing place rows on this walking point: keep (and update) the one of the same kind,
		// so its details, opening hours and menu stay attached; delete the others
		var oldStore, oldFacility int64
		if node > 0 {
			_ = tx.QueryRowContext(ctx, `SELECT store_id FROM indoor_nav_locations.stores WHERE navigation_node_id = ? ORDER BY store_id LIMIT 1`, node).Scan(&oldStore)
			_ = tx.QueryRowContext(ctx, `SELECT facility_id FROM indoor_nav_locations.facilities WHERE navigation_node_id = ? ORDER BY facility_id LIMIT 1`, node).Scan(&oldFacility)
			if in.Remove || !isStore {
				oldStore = 0
			}
			if in.Remove || isStore {
				oldFacility = 0
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM indoor_nav_locations.stores WHERE navigation_node_id = ? AND store_id <> ?`, node, oldStore); err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM indoor_nav_locations.facilities WHERE navigation_node_id = ? AND facility_id <> ?`, node, oldFacility)
			}
			if err != nil {
				log.Printf("beacon place clear: %v", err)
				fail(http.StatusInternalServerError, "Could not update the place. Check the server terminal.")
				return
			}
		}

		if in.Remove {
			if err = tx.Commit(); err != nil {
				fail(http.StatusInternalServerError, "Database error.")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"saved": true, "linked": false})
			return
		}

		if node == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO indoor_nav_locations.navigation_nodes
				(location_id, floor_id, node_code, x_coordinate, y_coordinate, node_type) VALUES (?, ?, ?, ?, ?, ?)`,
				b.location, b.floor, code, b.x, b.y, placeNodeType(in.Type))
			if err == nil {
				node, err = res.LastInsertId()
			}
			if err != nil {
				log.Printf("beacon place node: %v", err)
				fail(http.StatusInternalServerError, "Could not create the walking point. Check the server terminal.")
				return
			}
		} else {
			// the beacon may have moved: follow it, and reconnect below
			if _, err = tx.ExecContext(ctx, `UPDATE indoor_nav_locations.navigation_nodes
				SET x_coordinate = ?, y_coordinate = ?, node_type = ?, is_active = 1 WHERE node_id = ?`,
				b.x, b.y, placeNodeType(in.Type), node); err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM indoor_nav_locations.navigation_edges
					WHERE from_node_id = ? OR to_node_id = ?`, node, node)
			}
			if err != nil {
				log.Printf("beacon place move: %v", err)
				fail(http.StatusInternalServerError, "Could not update the walking point. Check the server terminal.")
				return
			}
		}

		// connect to the nearest corridor point (not another place point)
		var near int64
		var nx, ny float64
		err = tx.QueryRowContext(ctx, `SELECT node_id, x_coordinate, y_coordinate FROM indoor_nav_locations.navigation_nodes
			WHERE location_id = ? AND floor_id = ? AND is_active = 1 AND node_id <> ? AND node_code NOT LIKE 'PLACE\_B%'
			ORDER BY POW(x_coordinate - ?, 2) + POW(y_coordinate - ?, 2) LIMIT 1`,
			b.location, b.floor, node, b.x, b.y).Scan(&near, &nx, &ny)
		if err == sql.ErrNoRows {
			fail(http.StatusConflict, "This floor has no walking points yet, so the place can't be reached.")
			return
		}
		if err != nil {
			fail(http.StatusInternalServerError, "Database error.")
			return
		}
		d := math.Round(math.Hypot(nx-b.x, ny-b.y)*100) / 100
		if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO indoor_nav_locations.navigation_edges
			(location_id, from_node_id, to_node_id, distance_meters, edge_type, is_accessible) VALUES (?, ?, ?, ?, 'WALK', 1), (?, ?, ?, ?, 'WALK', 1)`,
			b.location, near, node, d, b.location, node, near, d); err != nil {
			log.Printf("beacon place edges: %v", err)
			fail(http.StatusInternalServerError, "Could not connect the place. Check the server terminal.")
			return
		}

		// the place itself
		var placeID int64
		kind := "facility"
		if isStore {
			kind = "store"
			if _, err = tx.ExecContext(ctx, `INSERT INTO indoor_nav_locations.store_categories (category_name) VALUES (?)
				ON DUPLICATE KEY UPDATE category_name = category_name`, in.Type); err != nil {
				fail(http.StatusInternalServerError, "Database error.")
				return
			}
			if oldStore > 0 {
				placeID = oldStore
				_, err = tx.ExecContext(ctx, `UPDATE indoor_nav_locations.stores SET store_name = ?, is_active = 1,
					category_id = (SELECT category_id FROM indoor_nav_locations.store_categories WHERE category_name = ? LIMIT 1)
					WHERE store_id = ?`, in.Name, in.Type, oldStore)
			} else {
				var res sql.Result
				res, err = tx.ExecContext(ctx, `INSERT INTO indoor_nav_locations.stores
					(location_id, floor_id, category_id, navigation_node_id, store_name)
					SELECT ?, ?, category_id, ?, ? FROM indoor_nav_locations.store_categories WHERE category_name = ? LIMIT 1`,
					b.location, b.floor, node, in.Name, in.Type)
				if err == nil {
					placeID, err = res.LastInsertId()
				}
			}
			if err != nil {
				log.Printf("beacon place store: %v", err)
				fail(http.StatusInternalServerError, "Could not save the shop. Check the server terminal.")
				return
			}
		} else {
			if oldFacility > 0 {
				placeID = oldFacility
				_, err = tx.ExecContext(ctx, `UPDATE indoor_nav_locations.facilities SET facility_name = ?, facility_type = ?, is_active = 1
					WHERE facility_id = ?`, in.Name, in.Type, oldFacility)
			} else {
				var res sql.Result
				res, err = tx.ExecContext(ctx, `INSERT INTO indoor_nav_locations.facilities
					(location_id, floor_id, navigation_node_id, facility_name, facility_type) VALUES (?, ?, ?, ?, ?)`,
					b.location, b.floor, node, in.Name, in.Type)
				if err == nil {
					placeID, err = res.LastInsertId()
				}
			}
			if err != nil {
				log.Printf("beacon place facility: %v", err)
				fail(http.StatusInternalServerError, "Could not save the place. Check the server terminal.")
				return
			}
		}

		if _, err = tx.ExecContext(ctx, `UPDATE indoor_nav_locations.beacons SET navigation_node_id = ? WHERE beacon_id = ?`, node, b.id); err != nil {
			fail(http.StatusInternalServerError, "Database error.")
			return
		}
		if err = tx.Commit(); err != nil {
			fail(http.StatusInternalServerError, "Database error.")
			return
		}
		log.Printf("Beacon %d is now the place %q (%s %s)", b.id, in.Name, kind, in.Type)
		_ = json.NewEncoder(w).Encode(map[string]any{"saved": true, "linked": true, "kind": kind, "type": in.Type,
			"name": in.Name, "place_id": placeID, "node_id": node})
	}
}
