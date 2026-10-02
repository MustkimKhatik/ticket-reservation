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
	}
	for _, s := range statements {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
