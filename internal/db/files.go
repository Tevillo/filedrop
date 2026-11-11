package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// File statuses.
const (
	StatusPending = "pending" // upload in progress
	StatusReady   = "ready"   // downloadable
	StatusDeleted = "deleted" // removed; never comes back
)

// Unlimited is the remaining_downloads value meaning "no download limit".
const Unlimited = -1

// Errors from file operations. The HTTP layer maps them to status codes.
var (
	ErrNotFound     = errors.New("file not found")                              // 404
	ErrExpired      = errors.New("file has expired")                            // 410
	ErrGone         = errors.New("file is deleted or has no downloads left")    // 410
	ErrBusy         = errors.New("all download slots for this file are in use") // 409
	ErrTooLarge     = errors.New("file is larger than the per-file limit")
	ErrStorageFull  = errors.New("not enough total storage left")
	ErrNotPending   = errors.New("file is not pending")
	ErrNoActiveSlot = errors.New("file has no download in progress")
)

// File is a files row.
type File struct {
	ID                 string
	APIKeyID           int64
	Status             string
	Locked             bool
	RemainingDownloads int64 // Unlimited (-1) means no limit
	CurrentDownloads   int64
	WrappedKey         []byte
	WrapNonce          []byte
	SizeBytes          int64
	OriginalName       string
	ContentType        string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// NewFile is what an upload provides when it starts.
type NewFile struct {
	ID                 string
	APIKeyID           int64
	RemainingDownloads int64
	WrappedKey         []byte
	WrapNonce          []byte
	SizeBytes          int64 // declared size, reserved against the total limit
	OriginalName       string
	ContentType        string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// Limits are the size caps, in bytes.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
}

const fileColumns = `id, api_key_id, status, locked, remaining_downloads, current_downloads,
	wrapped_key, wrap_nonce, size_bytes, original_name, content_type, created_at, expires_at`

type scanner interface{ Scan(dest ...any) error }

func scanFile(s scanner) (File, error) {
	var (
		f                File
		locked           int
		created, expires int64
	)
	err := s.Scan(&f.ID, &f.APIKeyID, &f.Status, &locked, &f.RemainingDownloads, &f.CurrentDownloads,
		&f.WrappedKey, &f.WrapNonce, &f.SizeBytes, &f.OriginalName, &f.ContentType, &created, &expires)
	if err != nil {
		return File{}, err
	}
	f.Locked = locked == 1
	f.CreatedAt = time.Unix(created, 0)
	f.ExpiresAt = time.Unix(expires, 0)
	return f, nil
}

// CreatePendingFile inserts a pending row, reserving f.SizeBytes of storage.
//
// The reservation is one INSERT ... SELECT: the row is only inserted if the
// sizes of all pending and ready files plus this one fit under the total.
// SQLite runs write statements one at a time, so two uploads racing for the
// last free space can't both succeed.
func (d *DB) CreatePendingFile(ctx context.Context, f NewFile, lim Limits) error {
	if f.SizeBytes > lim.MaxFileBytes {
		return ErrTooLarge
	}
	if f.RemainingDownloads < Unlimited || f.RemainingDownloads == 0 {
		return fmt.Errorf("remaining downloads must be -1 (unlimited) or at least 1, got %d", f.RemainingDownloads)
	}

	res, err := d.ExecContext(ctx, `
		INSERT INTO files (id, api_key_id, status, remaining_downloads, wrapped_key, wrap_nonce,
		                   size_bytes, original_name, content_type, created_at, expires_at)
		SELECT ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?
		WHERE (SELECT coalesce(sum(size_bytes), 0) FROM files WHERE status IN ('pending', 'ready')) + ? <= ?`,
		f.ID, f.APIKeyID, f.RemainingDownloads, f.WrappedKey, f.WrapNonce,
		f.SizeBytes, f.OriginalName, f.ContentType, f.CreatedAt.Unix(), f.ExpiresAt.Unix(),
		f.SizeBytes, lim.MaxTotalBytes)
	if err != nil {
		return fmt.Errorf("create pending file: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrStorageFull
	}
	return nil
}

// MarkReady moves a file from pending to ready once its upload has finished.
func (d *DB) MarkReady(ctx context.Context, id string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE files SET status = 'ready' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	return nil
}

// ActiveFile is a file that can still be downloaded, as shown in a key's
// status.
type ActiveFile struct {
	SizeBytes          int64
	ExpiresAt          time.Time
	RemainingDownloads int64 // Unlimited (-1) means no limit
}

// ActiveFiles returns apiKeyID's ready files that haven't expired or been
// used up, soonest-expiring first. Expired and used-up files stay in the
// table until the next sweep; they're left out here.
func (d *DB) ActiveFiles(ctx context.Context, apiKeyID int64, now time.Time) ([]ActiveFile, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT size_bytes, expires_at, remaining_downloads FROM files
		WHERE api_key_id = ? AND status = 'ready' AND expires_at > ? AND remaining_downloads != 0
		ORDER BY expires_at, created_at`, apiKeyID, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("active files: %w", err)
	}
	defer rows.Close()
	files := []ActiveFile{}
	for rows.Next() {
		var f ActiveFile
		var expires int64
		if err := rows.Scan(&f.SizeBytes, &expires, &f.RemainingDownloads); err != nil {
			return nil, fmt.Errorf("active files: %w", err)
		}
		f.ExpiresAt = time.Unix(expires, 0)
		files = append(files, f)
	}
	return files, rows.Err()
}

// ReservedBytes is the storage counted against the total limit: every
// pending and ready file, for all keys. CreatePendingFile uses the same sum.
func (d *DB) ReservedBytes(ctx context.Context) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx,
		`SELECT coalesce(sum(size_bytes), 0) FROM files WHERE status IN ('pending', 'ready')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reserved bytes: %w", err)
	}
	return n, nil
}

// GetFile returns a file in any status, or ErrNotFound.
func (d *DB) GetFile(ctx context.Context, id string) (File, error) {
	f, err := scanFile(d.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("get file: %w", err)
	}
	return f, nil
}

// AcquireSlot reserves one download slot before streaming starts.
//
// A single UPDATE both checks that a slot is free and takes it, and sets the
// lock flag in the same step when this was the last free slot, so two
// requests can't both take the final slot and the flag can't drift from the
// counts. If no row changes, a follow-up read explains why.
func (d *DB) AcquireSlot(ctx context.Context, id string, now time.Time) (File, error) {
	f, err := scanFile(d.QueryRowContext(ctx, `
		UPDATE files SET
			current_downloads = current_downloads + 1,
			locked = CASE
				WHEN remaining_downloads != -1 AND current_downloads + 1 >= remaining_downloads THEN 1
				ELSE 0
			END
		WHERE id = ?
		  AND status = 'ready'
		  AND locked = 0
		  AND expires_at > ?
		  AND (remaining_downloads = -1 OR current_downloads < remaining_downloads)
		RETURNING `+fileColumns, id, now.Unix()))
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return File{}, fmt.Errorf("acquire slot: %w", err)
	}

	// Nothing changed: work out why.
	cur, err := d.GetFile(ctx, id)
	if err != nil {
		return File{}, err
	}
	if err := unavailable(cur, now); err != nil {
		return File{}, err
	}
	return File{}, ErrBusy // a slot was taken between the UPDATE and the read
}

// CheckAvailable reports whether a download of id could start right now,
// without taking a slot. It returns nil or the error AcquireSlot would give.
// The answer can change a moment later; only AcquireSlot reserves anything.
func (d *DB) CheckAvailable(ctx context.Context, id string, now time.Time) error {
	f, err := d.GetFile(ctx, id)
	if err != nil {
		return err
	}
	return unavailable(f, now)
}

// unavailable explains why f can't be downloaded at now, or returns nil if a
// slot is free. Expiry is checked before the lock so an expired file reports
// "gone" rather than "busy".
func unavailable(f File, now time.Time) error {
	switch {
	case f.Status == StatusPending:
		return ErrNotFound
	case !f.ExpiresAt.After(now):
		return ErrExpired
	case f.Status == StatusDeleted, f.RemainingDownloads == 0:
		return ErrGone
	case f.Locked, f.RemainingDownloads != Unlimited && f.CurrentDownloads >= f.RemainingDownloads:
		return ErrBusy
	default:
		return nil
	}
}

// ReleaseSlot ends a download. On success it uses up one download (unless
// the file is unlimited); on failure it only frees the slot. The lock flag is
// recalculated in the same statement. exhausted is true when no downloads
// remain and none are in progress: the caller should hand the file to the
// cleaner.
func (d *DB) ReleaseSlot(ctx context.Context, id string, success bool, now time.Time) (exhausted bool, err error) {
	ok := 0
	if success {
		ok = 1
	}
	// In SQLite's SET clauses, column names refer to the values before this
	// update, so "new remaining" is spelled out where the lock is computed.
	var remaining, current int64
	err = d.QueryRowContext(ctx, `
		UPDATE files SET
			current_downloads = current_downloads - 1,
			remaining_downloads = CASE
				WHEN ?1 = 1 AND remaining_downloads != -1 THEN remaining_downloads - 1
				ELSE remaining_downloads
			END,
			locked = CASE
				WHEN status != 'ready' OR expires_at <= ?2 THEN 1
				WHEN remaining_downloads = -1 THEN 0
				WHEN remaining_downloads - ?1 <= current_downloads - 1 THEN 1
				ELSE 0
			END
		WHERE id = ?3 AND current_downloads > 0
		RETURNING remaining_downloads, current_downloads`,
		ok, now.Unix(), id).Scan(&remaining, &current)
	if errors.Is(err, sql.ErrNoRows) {
		if _, gerr := d.GetFile(ctx, id); gerr != nil {
			return false, gerr
		}
		return false, ErrNoActiveSlot
	}
	if err != nil {
		return false, fmt.Errorf("release slot: %w", err)
	}
	return remaining == 0 && current == 0, nil
}

// MarkDeleted sets a file to deleted and locks it. Doing it twice is fine;
// only a missing file is an error.
func (d *DB) MarkDeleted(ctx context.Context, id string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE files SET status = 'deleted', locked = 1 WHERE id = ? AND status != 'deleted'`, id)
	if err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := d.GetFile(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ResetSlots runs at startup. No download survives a restart, so every
// in-progress count goes back to 0 and each lock is recalculated.
func (d *DB) ResetSlots(ctx context.Context, now time.Time) error {
	_, err := d.ExecContext(ctx, `
		UPDATE files SET
			current_downloads = 0,
			locked = CASE
				WHEN status = 'deleted' OR expires_at <= ? THEN 1
				WHEN remaining_downloads = 0 THEN 1
				ELSE 0
			END`, now.Unix())
	if err != nil {
		return fmt.Errorf("reset slots: %w", err)
	}
	return nil
}
