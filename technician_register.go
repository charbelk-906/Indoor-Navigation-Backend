package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

const technicianRoleID = 1 // roles table: 1 = TECHNICIAN

var phonePattern = regexp.MustCompile(`^\+?[0-9 ]{7,20}$`)

type technicianRegisterRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	Phone      string `json:"phone"`
	EmployeeID string `json:"employee_id"`
	Company    string `json:"company"`
	AccessCode string `json:"access_code"`
}

// Creates an account AND gives it the TECHNICIAN role, but only when the
// access code matches the TECH_ACCESS_CODE environment variable.
// If TECH_ACCESS_CODE is not set, technician registration is disabled.
func registerTechnicianHandler(db *sql.DB) http.HandlerFunc {
	expectedCode := os.Getenv("TECH_ACCESS_CODE")

	return func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, message string) {
			sendJSON(w, status, map[string]string{"error": message})
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, "Use POST for registration")
			return
		}
		if expectedCode == "" {
			fail(http.StatusForbidden, "Technician registration is disabled on this server")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		defer r.Body.Close()

		var input technicianRegisterRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			fail(http.StatusBadRequest, "Invalid registration JSON")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(http.StatusBadRequest, "Send exactly one JSON object")
			return
		}

		// Check the access code first; slow down wrong guesses.
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(input.AccessCode)), []byte(expectedCode)) != 1 {
			time.Sleep(time.Second)
			fail(http.StatusForbidden, "Invalid technician access code")
			return
		}

		input.Email = strings.ToLower(strings.TrimSpace(input.Email))
		input.FirstName = strings.TrimSpace(input.FirstName)
		input.LastName = strings.TrimSpace(input.LastName)
		input.Phone = strings.TrimSpace(input.Phone)
		input.EmployeeID = strings.TrimSpace(input.EmployeeID)
		input.Company = strings.TrimSpace(input.Company)

		address, err := mail.ParseAddress(input.Email)
		if err != nil || address.Address != input.Email || utf8.RuneCountInString(input.Email) > 150 {
			fail(http.StatusBadRequest, "Enter a valid email, maximum 150 characters")
			return
		}
		if input.FirstName == "" || input.LastName == "" ||
			utf8.RuneCountInString(input.FirstName) > 100 || utf8.RuneCountInString(input.LastName) > 100 {
			fail(http.StatusBadRequest, "First and last name are required (max 100 characters)")
			return
		}
		if !phonePattern.MatchString(input.Phone) {
			fail(http.StatusBadRequest, "Enter a valid phone number")
			return
		}
		if input.EmployeeID == "" || utf8.RuneCountInString(input.EmployeeID) > 50 {
			fail(http.StatusBadRequest, "Employee ID is required (max 50 characters)")
			return
		}
		if utf8.RuneCountInString(input.Company) > 100 {
			fail(http.StatusBadRequest, "Company must not exceed 100 characters")
			return
		}
		if utf8.RuneCountInString(input.Password) < 15 || len([]byte(input.Password)) > 72 {
			fail(http.StatusBadRequest, "Password must contain at least 15 characters and at most 72 bytes")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("Password hashing failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		// User row + role row are saved together, or not at all.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("Transaction start failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}
		defer tx.Rollback()

		result, err := tx.ExecContext(ctx, `
			INSERT INTO indoor_nav_common.users
				(email, password_hash, first_name, last_name, phone, employee_id, company)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, input.Email, string(hash), input.FirstName, input.LastName, input.Phone, input.EmployeeID,
			sql.NullString{String: input.Company, Valid: input.Company != ""})
		if err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				if strings.Contains(mysqlErr.Message, "employee_id") {
					fail(http.StatusConflict, "This employee ID is already registered")
				} else {
					fail(http.StatusConflict, "An account with this email already exists")
				}
				return
			}
			log.Printf("Technician insert failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		userID, err := result.LastInsertId()
		if err != nil {
			log.Printf("Reading inserted ID failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO indoor_nav_common.user_roles (user_id, role_id) VALUES (?, ?)`,
			userID, technicianRoleID); err != nil {
			log.Printf("Role insert failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("Commit failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		sendJSON(w, http.StatusCreated, map[string]any{
			"message": "Technician account created",
			"user_id": userID,
			"email":   input.Email,
			"role":    "TECHNICIAN",
		})
	}
}
