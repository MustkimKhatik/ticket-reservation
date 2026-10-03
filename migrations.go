package main

import (
	"context"
	"database/sql"
)

func migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS shows (
			id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			name VARCHAR(255) NOT NULL,
			price_paise BIGINT NOT NULL,
			per_user_limit INT NOT NULL DEFAULT 4,
			total_seats INT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (id),
			CONSTRAINT chk_shows_price_positive CHECK (price_paise > 0),
			CONSTRAINT chk_shows_total_seats_positive CHECK (total_seats > 0)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS seats (
			show_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			seat_id VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			status ENUM('available','held','confirmed') NOT NULL DEFAULT 'available',
			user_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
			reservation_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
			held_until DATETIME(6) NULL,
			PRIMARY KEY (show_id, seat_id),
			INDEX idx_seats_show_status (show_id, status),
			CONSTRAINT fk_seats_show FOREIGN KEY (show_id) REFERENCES shows(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE INDEX IF NOT EXISTS idx_seats_show_user_status ON seats (show_id, user_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_seats_reservation_id ON seats (reservation_id)`,
		`CREATE TABLE IF NOT EXISTS reservations (
			id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			show_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			user_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			amount_paise BIGINT NOT NULL,
			status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			cancelled_at DATETIME(6) NULL,
			PRIMARY KEY (id),
			INDEX idx_reservations_show_user (show_id, user_id),
			CONSTRAINT fk_reservations_show FOREIGN KEY (show_id) REFERENCES shows(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`ALTER TABLE reservations ADD COLUMN IF NOT EXISTS cancelled_at DATETIME(6) NULL`,
		`CREATE TABLE IF NOT EXISTS reservation_seats (
			reservation_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			show_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			seat_id VARCHAR(16) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			PRIMARY KEY (reservation_id, seat_id),
			CONSTRAINT fk_reservation_seats_reservation FOREIGN KEY (reservation_id) REFERENCES reservations(id),
			CONSTRAINT fk_reservation_seats_seat FOREIGN KEY (show_id, seat_id) REFERENCES seats(show_id, seat_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`INSERT IGNORE INTO reservation_seats (reservation_id, show_id, seat_id)
			SELECT reservation_id, show_id, seat_id FROM seats WHERE reservation_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
			user_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			idem_key VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			show_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			request_hash BINARY(32) NOT NULL,
			response_json JSON NULL,
			status_code SMALLINT UNSIGNED NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY uq_idempotency_user_key (user_id, idem_key),
			CONSTRAINT fk_idempotency_show FOREIGN KEY (show_id) REFERENCES shows(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS user_show_locks (
			show_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			user_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
			PRIMARY KEY (show_id, user_id),
			CONSTRAINT fk_user_show_locks_show FOREIGN KEY (show_id) REFERENCES shows(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
