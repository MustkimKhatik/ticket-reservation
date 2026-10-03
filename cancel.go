package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"time"
)

type cancelResponse struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

type cancelResult struct {
	body      []byte
	cancelled bool
	notFound  bool
}

func (a *application) cancelReservation(w http.ResponseWriter, r *http.Request) {
	ident, ok := r.Context().Value(identityKey{}).(identity)
	if !ok || ident.UserID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
		return
	}
	reservationID := r.PathValue("id")
	if len(reservationID) != 36 || !looksLikeUUID(reservationID) {
		writeError(w, http.StatusNotFound, "reservation_not_found", "Reservation not found")
		return
	}
	if !a.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Service is not ready")
		return
	}
	for retry := 0; ; retry++ {
		result, err := a.cancelReservationOnce(r.Context(), ident.UserID, reservationID)
		if err != nil {
			if isMySQLError(err, 1213) && retry < 3 {
				timer := time.NewTimer(time.Duration(10+rand.Intn(41)) * time.Millisecond)
				select {
				case <-r.Context().Done():
					timer.Stop()
					a.dbError(w, r, r.Context().Err())
					return
				case <-timer.C:
				}
				continue
			}
			a.dbError(w, r, err)
			return
		}
		if result.notFound {
			writeError(w, http.StatusNotFound, "reservation_not_found", "Reservation not found")
			return
		}
		if result.cancelled {
			a.metrics.incReservationCancelled()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.body)
		return
	}
}

func (a *application) cancelReservationOnce(ctx context.Context, userID, reservationID string) (cancelResult, error) {
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, a.cfg.dbAcquireTimeout)
	conn, err := a.db.Conn(acquireCtx)
	cancelAcquire()
	if err != nil {
		return cancelResult{}, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return cancelResult{}, err
	}
	defer tx.Rollback()

	stmtCtx, cancel := context.WithTimeout(ctx, a.cfg.statementTimeout)
	var showID, ownerID, status string
	var amount int64
	err = tx.QueryRowContext(stmtCtx, `SELECT show_id, user_id, amount_paise, status FROM reservations WHERE id=? FOR UPDATE`, reservationID).Scan(&showID, &ownerID, &amount, &status)
	cancel()
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ownerID != userID) {
		return cancelResult{notFound: true}, nil // Same response for unknown and non-owner IDs.
	}
	if err != nil {
		return cancelResult{}, err
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	rows, err := tx.QueryContext(stmtCtx, `SELECT seat_id FROM reservation_seats WHERE reservation_id=? ORDER BY seat_id`, reservationID)
	if err != nil {
		cancel()
		return cancelResult{}, err
	}
	seats := make([]string, 0, 4)
	for rows.Next() {
		var seat string
		if err := rows.Scan(&seat); err != nil {
			_ = rows.Close()
			cancel()
			return cancelResult{}, err
		}
		seats = append(seats, seat)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		cancel()
		return cancelResult{}, err
	}
	if err := rows.Close(); err != nil {
		cancel()
		return cancelResult{}, err
	}
	cancel()
	out := cancelResponse{ReservationID: reservationID, ShowID: showID, UserID: ownerID, Seats: seats, AmountPaise: amount, Status: "cancelled"}
	body, err := json.Marshal(out)
	if err != nil {
		return cancelResult{}, err
	}
	if status == "cancelled" {
		if err := tx.Commit(); err != nil {
			return cancelResult{}, err
		}
		return cancelResult{body: body}, nil
	}
	if status != "confirmed" {
		return cancelResult{}, errors.New("reservation is not cancellable")
	}

	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	update, err := tx.ExecContext(stmtCtx, `UPDATE seats SET status='available', user_id=NULL, reservation_id=NULL WHERE show_id=? AND reservation_id=? AND status='confirmed'`, showID, reservationID)
	cancel()
	if err != nil {
		return cancelResult{}, err
	}
	changed, err := update.RowsAffected()
	if err != nil {
		return cancelResult{}, err
	}
	if changed != int64(len(seats)) {
		a.logger.Error("cancelled reservation seat count mismatch", "reservation_id", reservationID, "expected", len(seats), "released", changed)
	}
	stmtCtx, cancel = context.WithTimeout(ctx, a.cfg.statementTimeout)
	_, err = tx.ExecContext(stmtCtx, `UPDATE reservations SET status='cancelled', cancelled_at=NOW(6) WHERE id=?`, reservationID)
	cancel()
	if err != nil {
		return cancelResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return cancelResult{}, err
	}
	return cancelResult{body: body, cancelled: true}, nil
}
