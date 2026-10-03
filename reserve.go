package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

type reserveRequest struct {
	Seats          []string        `json:"seats"`
	IdempotencyKey json.RawMessage `json:"idempotency_key"`
}

type reserveResponse struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

type reserveResult struct {
	status         int
	body           []byte
	errorCode      string
	errorMessage   string
	confirmed      bool
	replayed       bool
	declinedReason string
}

func (a *application) reserve(w http.ResponseWriter, r *http.Request) {
	ident, ok := r.Context().Value(identityKey{}).(identity)
	if !ok || ident.UserID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
		return
	}
	showID := r.PathValue("id")
	if len(showID) != 36 || !looksLikeUUID(showID) {
		writeError(w, http.StatusNotFound, "show_not_found", "Show not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body) // Unknown fields, including user_id, are deliberately ignored.
	var in reserveRequest
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON")
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body must contain one JSON object")
		return
	}
	bodyKeyProvided := len(in.IdempotencyKey) != 0
	bodyKey := ""
	if bodyKeyProvided {
		if err := json.Unmarshal(in.IdempotencyKey, &bodyKey); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be a string")
			return
		}
	}
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if bodyKeyProvided && headerKey != "" && bodyKey != headerKey {
		writeError(w, http.StatusBadRequest, "idempotency_key_mismatch", "Body and header idempotency keys must match")
		return
	}
	key := bodyKey
	if key == "" {
		key = headerKey
	}
	if strings.TrimSpace(key) == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "An idempotency key is required")
		return
	}
	if utf8.RuneCountInString(key) > 255 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency key must be at most 255 characters")
		return
	}
	if len(in.Seats) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_seats", "seats must be non-empty")
		return
	}
	seats := make([]string, len(in.Seats))
	seen := make(map[string]struct{}, len(in.Seats))
	for i, raw := range in.Seats {
		seat := NormalizeSeatID(raw)
		if seat == "" || len([]rune(seat)) > 16 {
			writeError(w, http.StatusBadRequest, "invalid_seat", "each seat must be non-empty and at most 16 characters after normalization")
			return
		}
		if _, exists := seen[seat]; exists {
			writeError(w, http.StatusBadRequest, "duplicate_seat", "seat IDs must be unique after normalization")
			return
		}
		seen[seat] = struct{}{}
		seats[i] = seat
	}
	sort.Strings(seats)
	if !a.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready")
		return
	}
	requestHash := reservationRequestHash(showID, seats)
	for retry := 0; ; retry++ {
		result, err := a.reserveOnce(r.Context(), ident.UserID, showID, key, seats, requestHash)
		if err != nil {
			if isMySQLError(err, 1213) && retry < 3 {
				backoff := time.Duration(10+rand.Intn(41)) * time.Millisecond
				timer := time.NewTimer(backoff)
				select {
				case <-r.Context().Done():
					timer.Stop()
					a.dbError(w, r, r.Context().Err())
					return
				case <-timer.C:
				}
				continue
			}
			if isMySQLError(err, 1062) {
				writeError(w, http.StatusConflict, "duplicate_key", "A reservation uniqueness conflict occurred")
				return
			}
			a.dbError(w, r, err)
			return
		}
		if result.confirmed {
			a.metrics.incReservationConfirmed()
		}
		if result.declinedReason != "" {
			a.metrics.incReservationDeclined(result.declinedReason)
		}
		if result.replayed {
			a.metrics.incReservationDeclined("idempotent_replay")
		}
		if result.errorCode != "" {
			writeError(w, result.status, result.errorCode, result.errorMessage)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if result.replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.WriteHeader(result.status)
		_, _ = w.Write(result.body)
		return
	}
}

func (a *application) reserveOnce(ctx context.Context, userID, showID, idemKey string, seats []string, requestHash []byte) (reserveResult, error) {
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, a.cfg.dbAcquireTimeout)
	conn, err := a.db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		return reserveResult{}, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return reserveResult{}, err
	}
	defer tx.Rollback()

	stmtCtx, cancel := context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, `INSERT INTO idempotency_keys
		(user_id, idem_key, show_id, request_hash, response_json, status_code) VALUES (?, ?, ?, ?, NULL, NULL)`, userID, idemKey, showID, requestHash)
	cancel()
	if isMySQLError(err, 1062) {
		_ = tx.Rollback()
		return readIdempotencyReplay(ctx, conn, userID, idemKey, requestHash, a.cfg.statementTimeout)
	}
	if isMySQLError(err, 1452) {
		return reserveResult{status: http.StatusNotFound, errorCode: "show_not_found", errorMessage: "Show not found"}, nil
	}
	if err != nil {
		return reserveResult{}, err
	}

	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, `INSERT IGNORE INTO user_show_locks (show_id, user_id) VALUES (?, ?)`, showID, userID)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	var lockedShow string
	err = tx.QueryRowContext(stmtCtx, `SELECT show_id FROM user_show_locks WHERE show_id=? AND user_id=? FOR UPDATE`, showID, userID).Scan(&lockedShow)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}

	var price int64
	var perUserLimit, totalSeats int
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	err = tx.QueryRowContext(stmtCtx, `SELECT price_paise, per_user_limit, total_seats FROM shows WHERE id=?`, showID).Scan(&price, &perUserLimit, &totalSeats)
	cancel()
	if errors.Is(err, sql.ErrNoRows) {
		return reserveResult{status: http.StatusNotFound, errorCode: "show_not_found", errorMessage: "Show not found"}, nil
	}
	if err != nil {
		return reserveResult{}, err
	}
	if len(seats) > totalSeats {
		_ = tx.Rollback()
		return reserveResult{status: http.StatusNotFound, errorCode: "seat_not_found", errorMessage: "A requested seat does not exist for this show"}, nil
	}
	var presentSeatCount int
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	err = tx.QueryRowContext(stmtCtx, `SELECT COUNT(*) FROM seats WHERE show_id=? AND seat_id IN (`+placeholders(len(seats))+`)`, append([]any{showID}, stringArgs(seats)...)...).Scan(&presentSeatCount)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	if presentSeatCount != len(seats) {
		_ = tx.Rollback()
		return reserveResult{status: http.StatusNotFound, errorCode: "seat_not_found", errorMessage: "A requested seat does not exist for this show"}, nil
	}
	if int64(len(seats)) > math.MaxInt64/price {
		return reserveResult{status: http.StatusBadRequest, errorCode: "amount_overflow", errorMessage: "Reservation amount exceeds the supported range"}, nil
	}
	var userSeatCount int64
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	err = tx.QueryRowContext(stmtCtx, `SELECT COUNT(*) FROM seats WHERE show_id=? AND user_id=? AND status IN ('held','confirmed')`, showID, userID).Scan(&userSeatCount)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	if userSeatCount+int64(len(seats)) > int64(perUserLimit) {
		_ = tx.Rollback()
		return reserveResult{status: http.StatusConflict, errorCode: "per_user_limit_exceeded", errorMessage: "Reservation exceeds the per-user seat limit", declinedReason: "per_user_limit"}, nil
	}

	// Point-lock each requested seat in normalized PK order. The UPDATE below
	// remains the conditional, all-or-nothing availability decision.
	for _, seat := range seats {
		var found string
		stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
		err = tx.QueryRowContext(stmtCtx, `SELECT seat_id FROM seats FORCE INDEX (PRIMARY) WHERE show_id=? AND seat_id=? FOR UPDATE`, showID, seat).Scan(&found)
		cancel()
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return reserveResult{status: http.StatusNotFound, errorCode: "seat_not_found", errorMessage: "A requested seat does not exist for this show"}, nil
		}
		if err != nil {
			return reserveResult{}, err
		}
	}

	reservationID := newID()
	query := `UPDATE seats SET status='confirmed', user_id=?, reservation_id=? WHERE show_id=? AND status='available' AND seat_id IN (` + placeholders(len(seats)) + `)`
	args := make([]any, 0, len(seats)+3)
	args = append(args, userID, reservationID, showID)
	for _, seat := range seats {
		args = append(args, seat)
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	updateResult, err := tx.ExecContext(stmtCtx, query, args...)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	changed, err := updateResult.RowsAffected()
	if err != nil {
		return reserveResult{}, err
	}
	if changed != int64(len(seats)) {
		_ = tx.Rollback()
		return reserveResult{status: http.StatusConflict, errorCode: "seat_taken", errorMessage: "One or more requested seats are unavailable", declinedReason: "seat_taken"}, nil
	}

	amount := price * int64(len(seats))
	out := reserveResponse{ReservationID: reservationID, ShowID: showID, UserID: userID, Seats: seats, AmountPaise: amount, Status: "confirmed"}
	body, err := json.Marshal(out)
	if err != nil {
		return reserveResult{}, err
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, `INSERT INTO reservations (id, show_id, user_id, amount_paise, status) VALUES (?, ?, ?, ?, 'confirmed')`, reservationID, showID, userID, amount)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	seatValues := make([]string, len(seats))
	seatArgs := make([]any, 0, len(seats)*3)
	for i, seat := range seats {
		seatValues[i] = "(?,?,?)"
		seatArgs = append(seatArgs, reservationID, showID, seat)
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, `INSERT INTO reservation_seats (reservation_id, show_id, seat_id) VALUES `+strings.Join(seatValues, ","), seatArgs...)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	updated, err := tx.ExecContext(stmtCtx, `UPDATE idempotency_keys SET response_json=?, status_code=201 WHERE user_id=? AND idem_key=?`, string(body), userID, idemKey)
	cancel()
	if err != nil {
		return reserveResult{}, err
	}
	updatedRows, err := updated.RowsAffected()
	if err != nil {
		return reserveResult{}, err
	}
	if updatedRows != 1 {
		return reserveResult{}, errors.New("idempotency row was not updated")
	}
	if err = tx.Commit(); err != nil {
		return reserveResult{}, err
	}
	return reserveResult{status: http.StatusCreated, body: body, confirmed: true}, nil
}

func readIdempotencyReplay(ctx context.Context, conn *sql.Conn, userID, idemKey string, requestHash []byte, timeout time.Duration) (reserveResult, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var existingHash, response []byte
	var status sql.NullInt64
	err := conn.QueryRowContext(readCtx, `SELECT request_hash, response_json, status_code FROM idempotency_keys WHERE user_id=? AND idem_key=?`, userID, idemKey).Scan(&existingHash, &response, &status)
	if err != nil {
		return reserveResult{}, err
	}
	if !bytes.Equal(existingHash, requestHash) {
		return reserveResult{status: http.StatusConflict, errorCode: "idempotency_key_conflict", errorMessage: "Idempotency key was already used for a different request"}, nil
	}
	if !status.Valid || len(response) == 0 {
		return reserveResult{}, errors.New("committed idempotency row is incomplete")
	}
	return reserveResult{status: int(status.Int64), body: response, replayed: true}, nil
}

func reservationRequestHash(showID string, seats []string) []byte {
	h := sha256.New()
	writeHashPart := func(part string) {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	writeHashPart(showID)
	for _, seat := range seats {
		writeHashPart(seat)
	}
	return h.Sum(nil)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func isMySQLError(err error, number uint16) bool {
	var dbErr *mysql.MySQLError
	return errors.As(err, &dbErr) && dbErr.Number == number
}

func looksLikeUUID(id string) bool {
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			return false
		}
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
