// Package store saves files encrypted on disk and streams them back out.
// It ties the pieces together: the database row (with its storage
// reservation and download slots), the per-file data key wrapped by the master
// key, and sio's chunked encryption of the file contents.
//
// The plaintext never touches disk: Save encrypts as the input is read, and
// Open's Download decrypts as it writes to the caller.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/sio"

	"github.com/Tevillo/encryptor-api/internal/db"
	"github.com/Tevillo/encryptor-api/internal/fileid"
	"github.com/Tevillo/encryptor-api/internal/keywrap"
)

// MaxTTL is the longest a file may live.
// CHANGE MAX TIME HERE
const MaxTTL = 48 * time.Hour

var (
	// ErrSizeMismatch means the input wasn't exactly the declared size.
	ErrSizeMismatch = errors.New("file size does not match the declared size")
	// ErrBadUpload means the upload's settings are invalid.
	ErrBadUpload = errors.New("invalid upload")
)

// Store saves and opens encrypted files.
type Store struct {
	db     *db.DB
	dir    string
	master []byte
	limits db.Limits
	now    func() time.Time // replaceable in tests
}

// New returns a Store keeping encrypted files in dir (created with mode 0700).
func New(d *db.DB, dir string, master []byte, lim db.Limits) (*Store, error) {
	if len(master) != keywrap.KeySize {
		return nil, fmt.Errorf("master key must be %d bytes", keywrap.KeySize)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	return &Store{db: d, dir: dir, master: master, limits: lim, now: time.Now}, nil
}

// sioConfig pins the format to DARE 2.0 with AES-256-GCM. Pinning MinVersion
// stops decryption from accepting the older 1.0 format.
func sioConfig(dataKey []byte) sio.Config {
	return sio.Config{
		MinVersion:   sio.Version20,
		MaxVersion:   sio.Version20,
		CipherSuites: []byte{sio.AES_GCM},
		Key:          dataKey,
	}
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id) }

// Upload describes a file being saved.
type Upload struct {
	APIKeyID     int64
	OriginalName string
	ContentType  string
	Size         int64         // declared size in bytes; the input must match exactly
	Downloads    int64         // db.Unlimited (-1) or at least 1
	TTL          time.Duration // how long until it expires; at most MaxTTL
}

// Save encrypts everything read from r into a new file and returns its ID.
//
// Steps, matching the upload design: reserve storage with a pending row →
// stream r through sio encryption into <data dir>/<id> → check exactly
// u.Size bytes arrived → flush to disk → mark ready. On any failure the
// partial file is removed and the row marked deleted, which frees the
// reservation.
func (s *Store) Save(ctx context.Context, r io.Reader, u Upload) (id string, err error) {
	if u.TTL <= 0 || u.TTL > MaxTTL {
		return "", fmt.Errorf("%w: expiry must be between 1s and %s", ErrBadUpload, MaxTTL)
	}
	if u.OriginalName == "" || u.Size < 0 {
		return "", fmt.Errorf("%w: name and size are required", ErrBadUpload)
	}

	id, err = fileid.New()
	if err != nil {
		return "", err
	}
	dataKey, err := keywrap.NewDataKey()
	if err != nil {
		return "", err
	}
	wrapped, nonce, err := keywrap.Wrap(s.master, dataKey, id)
	if err != nil {
		return "", err
	}

	now := s.now()
	err = s.db.CreatePendingFile(ctx, db.NewFile{
		ID:                 id,
		APIKeyID:           u.APIKeyID,
		RemainingDownloads: u.Downloads,
		WrappedKey:         wrapped,
		WrapNonce:          nonce,
		SizeBytes:          u.Size,
		OriginalName:       u.OriginalName,
		ContentType:        u.ContentType,
		CreatedAt:          now,
		ExpiresAt:          now.Add(u.TTL),
	}, s.limits)
	if err != nil {
		return "", err // nothing written yet, nothing to clean up
	}

	// Keep our own copy: `return "", err` sets the named result id to ""
	// before this deferred cleanup runs.
	fileID, path := id, s.path(id)
	defer func() {
		if err != nil {
			os.Remove(path)
			// Use a context that isn't cancelled, so cleanup still runs if
			// the caller's context (e.g. a dropped HTTP request) was.
			s.db.MarkDeleted(context.WithoutCancel(ctx), fileID)
		}
	}()

	// O_EXCL: never overwrite an existing file, even if an ID somehow repeated.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create encrypted file: %w", err)
	}

	// Read at most one byte more than declared, so "too long" is detectable
	// without reading an unbounded stream.
	counted := &countingReader{r: io.LimitReader(r, u.Size+1)}
	if _, err = sio.Encrypt(f, counted, sioConfig(dataKey)); err != nil {
		f.Close()
		return "", fmt.Errorf("encrypt: %w", err)
	}
	if counted.n != u.Size {
		f.Close()
		return "", fmt.Errorf("%w: declared %d bytes, got %d", ErrSizeMismatch, u.Size, min(counted.n, u.Size+1))
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("flush encrypted file: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close encrypted file: %w", err)
	}
	if err = s.db.MarkReady(ctx, id); err != nil {
		return "", err
	}
	return id, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Download is a file whose download slot has been taken. The caller must
// call Finish exactly once.
type Download struct {
	File db.File
	Size int64 // decrypted size, for Content-Length

	s       *Store
	f       *os.File
	dataKey []byte
}

// KeyStatus summarizes one API key's downloadable files and the service's
// storage.
type KeyStatus struct {
	Files          []StatusFile // soonest-expiring first
	UsedBytes      int64        // total size of Files
	LimitBytes     int64        // service-wide total limit
	RemainingBytes int64        // service-wide space left for uploads
}

// StatusFile is one downloadable file, without anything identifying it.
type StatusFile struct {
	SizeBytes     int64
	ExpiresIn     time.Duration
	DownloadsLeft int64 // db.Unlimited (-1) means no limit
}

// Status reports apiKeyID's files that can still be downloaded, and how much
// storage the whole service has left.
func (s *Store) Status(ctx context.Context, apiKeyID int64) (KeyStatus, error) {
	now := s.now()
	active, err := s.db.ActiveFiles(ctx, apiKeyID, now)
	if err != nil {
		return KeyStatus{}, err
	}
	reserved, err := s.db.ReservedBytes(ctx)
	if err != nil {
		return KeyStatus{}, err
	}
	st := KeyStatus{
		Files:      make([]StatusFile, 0, len(active)),
		LimitBytes: s.limits.MaxTotalBytes,
		// Can go below zero if the limit was lowered after files were stored.
		RemainingBytes: max(s.limits.MaxTotalBytes-reserved, 0),
	}
	for _, f := range active {
		st.UsedBytes += f.SizeBytes
		st.Files = append(st.Files, StatusFile{
			SizeBytes:     f.SizeBytes,
			ExpiresIn:     f.ExpiresAt.Sub(now),
			DownloadsLeft: f.RemainingDownloads,
		})
	}
	return st, nil
}

// Check reports whether id could be downloaded right now without taking a
// slot: nil, or one of the errors Open would return.
func (s *Store) Check(ctx context.Context, id string) error {
	if !fileid.Valid(id) {
		return db.ErrNotFound
	}
	return s.db.CheckAvailable(ctx, id, s.now())
}

// Open takes a download slot for id and prepares to decrypt it. Errors from
// db.AcquireSlot (ErrNotFound, ErrExpired, ErrGone, ErrBusy) pass through.
func (s *Store) Open(ctx context.Context, id string) (_ *Download, err error) {
	if !fileid.Valid(id) {
		return nil, db.ErrNotFound
	}
	file, err := s.db.AcquireSlot(ctx, id, s.now())
	if err != nil {
		return nil, err
	}
	// From here on we hold a slot; give it back if anything fails.
	defer func() {
		if err != nil {
			s.db.ReleaseSlot(context.WithoutCancel(ctx), id, false, s.now())
		}
	}()

	dataKey, err := keywrap.Unwrap(s.master, file.WrappedKey, file.WrapNonce, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("open encrypted file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size, err := sio.DecryptedSize(uint64(info.Size()))
	if err == nil && int64(size) != file.SizeBytes {
		err = fmt.Errorf("decrypts to %d bytes but %d were stored", size, file.SizeBytes)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("encrypted file is damaged: %w", err)
	}
	return &Download{File: file, Size: int64(size), s: s, f: f, dataKey: dataKey}, nil
}

// WriteTo decrypts the file into w. If the stored file was altered, sio
// detects it and returns an error; chunks before the damaged one may already
// have been written to w.
func (dl *Download) WriteTo(w io.Writer) (int64, error) {
	// sio writes nothing for empty input and won't decrypt nothing, so an
	// empty file is handled here. Open already confirmed the stored size is 0.
	if dl.Size == 0 {
		return 0, nil
	}
	n, err := sio.Decrypt(w, dl.f, sioConfig(dl.dataKey))
	if err != nil {
		return n, fmt.Errorf("decrypt: %w", err)
	}
	return n, nil
}

// Finish releases the download slot. success=true uses up a download;
// false only frees the slot. exhausted reports that no downloads remain, so
// the file should be handed to the cleaner.
func (dl *Download) Finish(ctx context.Context, success bool) (exhausted bool, err error) {
	dl.f.Close()
	return dl.s.db.ReleaseSlot(context.WithoutCancel(ctx), dl.File.ID, success, dl.s.now())
}

// StillAvailable reports ErrGone if the file was deleted (for example by
// revoking its key) after this download started. Long downloads call it
// periodically so a revoke cuts them off.
func (dl *Download) StillAvailable(ctx context.Context) error {
	f, err := dl.s.db.GetFile(ctx, dl.File.ID)
	if err != nil {
		return err
	}
	if f.Status == db.StatusDeleted {
		return db.ErrGone
	}
	return nil
}
