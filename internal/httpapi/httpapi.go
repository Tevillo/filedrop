// Package httpapi is the HTTP interface:
//
//	PUT /filedrop/{name}   upload (API key required); the body is the file
//	GET /filedrop/{id}     share link: a small page whose script starts the download
//	GET /filedrop/{id}?raw the file itself (no key needed; uses up a download)
//	GET /filedrop/api/status  the key's downloadable files and storage, as JSON
//
// Uploads are the raw request body, so Content-Length is the declared size
// used to reserve storage before any byte is read.
package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Tevillo/encryptor-api/internal/apikey"
	"github.com/Tevillo/encryptor-api/internal/db"
	"github.com/Tevillo/encryptor-api/internal/store"
)

// Defaults for timeouts; tests shorten them.
const (
	DefaultStallTimeout  = 60 * time.Second // a transfer with no progress for this long is cut off
	DefaultRevokeCheck   = 10 * time.Second // how often a running download re-checks it wasn't revoked
	DefaultDownloadLimit = 1
	maxNameBytes         = 255
)

// Server handles HTTP requests.
type Server struct {
	db           *db.DB
	store        *store.Store
	publicURL    string // e.g. https://example.com/filedrop
	maxFileBytes int64
	log          *slog.Logger

	StallTimeout time.Duration
	RevokeCheck  time.Duration
}

// New returns a Server. publicURL is the start of every share link.
func New(d *db.DB, s *store.Store, publicURL string, maxFileBytes int64, log *slog.Logger) *Server {
	return &Server{
		db: d, store: s, publicURL: strings.TrimRight(publicURL, "/"), maxFileBytes: maxFileBytes, log: log,
		StallTimeout: DefaultStallTimeout, RevokeCheck: DefaultRevokeCheck,
	}
}

// Handler returns the routes wrapped in request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /filedrop/{name}", s.upload)
	mux.HandleFunc("GET /filedrop/{id}", s.download)
	mux.HandleFunc("GET /filedrop/api/status", s.status)
	return s.logRequests(mux)
}

// fail writes a one-line plain-text error.
func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, "error: %s\n", msg)
}

// short shortens a file ID for logs; the full ID is a secret.
func short(id string) string {
	if len(id) > 6 {
		return id[:6] + "…"
	}
	return id
}

// ---- upload ----

func (s *Server) authenticate(r *http.Request) (db.APIKey, bool) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		return db.APIKey{}, false
	}
	k, err := s.db.GetAPIKeyByHash(r.Context(), apikey.Hash(strings.TrimSpace(key)))
	if err != nil || k.Revoked {
		if err != nil && !errors.Is(err, db.ErrKeyNotFound) {
			s.log.Error("api key lookup failed", "err", err)
		}
		return db.APIKey{}, false
	}
	return k, true
}

// validName checks the uploaded file's name.
func validName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return errors.New("a file name is required (end the URL with / when using curl -T)")
	case len(name) > maxNameBytes:
		return fmt.Errorf("file name is longer than %d bytes", maxNameBytes)
	case !utf8.ValidString(name):
		return errors.New("file name is not valid UTF-8")
	case strings.ContainsAny(name, `/\`):
		return errors.New("file name may not contain slashes")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("file name may not contain control characters")
		}
	}
	return nil
}

// knownSettings are the only query keys an upload accepts. Anything else is
// rejected, so a typo like "expire=2h" fails loudly instead of silently
// becoming the default.
var knownSettings = map[string]bool{"expires": true, "downloads": true}

func parseSettings(r *http.Request) (ttl time.Duration, downloads int64, err error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, errors.New("malformed query string")
	}
	for k, vs := range q {
		if !knownSettings[k] {
			return 0, 0, fmt.Errorf("unknown setting %q (valid: expires, downloads)", k)
		}
		if len(vs) > 1 {
			return 0, 0, fmt.Errorf("setting %q given more than once", k)
		}
	}
	ttl = store.MaxTTL
	if v := q.Get("expires"); v != "" {
		ttl, err = time.ParseDuration(v)
		if err != nil || ttl < time.Second || ttl > store.MaxTTL {
			return 0, 0, fmt.Errorf("expires must be a duration from 1s to %s, such as 2h or 30m", store.MaxTTL)
		}
	}
	downloads = DefaultDownloadLimit
	if v := q.Get("downloads"); v != "" {
		downloads, err = strconv.ParseInt(v, 10, 64)
		if err != nil || (downloads != db.Unlimited && downloads < 1) {
			return 0, 0, errors.New("downloads must be -1 (unlimited) or at least 1")
		}
	}
	return ttl, downloads, nil
}

// contentType uses the request's type if it's a real one, else guesses from
// the file extension. curl sends no type with -T, and
// application/x-www-form-urlencoded with --data-binary.
func contentType(r *http.Request, name string) string {
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/x-www-form-urlencoded" {
		if _, _, err := mime.ParseMediaType(ct); err == nil {
			return ct
		}
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="filedrop"`)
		fail(w, http.StatusUnauthorized, "missing, unknown or revoked API key")
		return
	}
	// These checks happen before any of the body is read. curl waits for
	// the server's go-ahead before sending large bodies, so a rejected upload
	// never transfers the file.
	if r.ContentLength < 0 {
		fail(w, http.StatusLengthRequired, "Content-Length is required; upload a file, not a stream")
		return
	}
	if r.ContentLength > s.maxFileBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file is larger than the %d-byte limit", s.maxFileBytes))
		return
	}
	name := r.PathValue("name")
	if err := validName(name); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ttl, downloads, err := parseSettings(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	body := &stallReader{r: r.Body, rc: http.NewResponseController(w), timeout: s.StallTimeout}
	id, err := s.store.Save(r.Context(), body, store.Upload{
		APIKeyID:     key.ID,
		OriginalName: name,
		ContentType:  contentType(r, name),
		Size:         r.ContentLength,
		Downloads:    downloads,
		TTL:          ttl,
	})
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	switch {
	case err == nil:
	case errors.Is(err, db.ErrTooLarge):
		fail(w, http.StatusRequestEntityTooLarge, "file is larger than the per-file limit")
		return
	case errors.Is(err, db.ErrStorageFull):
		fail(w, http.StatusInsufficientStorage, "not enough storage left; try again after some files expire")
		return
	case errors.Is(err, store.ErrBadUpload):
		fail(w, http.StatusBadRequest, err.Error())
		return
	default:
		// Usually the client disconnected or stalled mid-upload; Save has
		// already removed the partial file.
		s.log.Warn("upload failed", "key", key.Name, "err", err)
		fail(w, http.StatusBadRequest, "upload did not complete")
		return
	}

	s.log.Info("file uploaded", "file", short(id), "key", key.Name, "bytes", r.ContentLength,
		"downloads", downloads, "expires_in", ttl.String())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "%s/%s\n", s.publicURL, id)
}

// ---- status ----

type statusResponse struct {
	Storage struct {
		UsedBytes             int64 `json:"used_bytes"`
		ServiceLimitBytes     int64 `json:"service_limit_bytes"`
		ServiceRemainingBytes int64 `json:"service_remaining_bytes"`
	} `json:"storage"`
	Files struct {
		Count int          `json:"count"`
		Items []statusItem `json:"items"`
	} `json:"files"`
}

type statusItem struct {
	SizeBytes        int64  `json:"size_bytes"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	DownloadsLeft    *int64 `json:"downloads_left"` // null means unlimited
}

// status reports the calling key's downloadable files (anonymously: no IDs
// or names) and the service's remaining storage.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="filedrop"`)
		fail(w, http.StatusUnauthorized, "missing, unknown or revoked API key")
		return
	}
	st, err := s.store.Status(r.Context(), key.ID)
	if err != nil {
		s.log.Error("status failed", "key", key.Name, "err", err)
		fail(w, http.StatusInternalServerError, "could not read status")
		return
	}
	var resp statusResponse
	resp.Storage.UsedBytes = st.UsedBytes
	resp.Storage.ServiceLimitBytes = st.LimitBytes
	resp.Storage.ServiceRemainingBytes = st.RemainingBytes
	resp.Files.Count = len(st.Files)
	resp.Files.Items = make([]statusItem, 0, len(st.Files))
	for _, f := range st.Files {
		item := statusItem{SizeBytes: f.SizeBytes, ExpiresInSeconds: int64(f.ExpiresIn / time.Second)}
		if f.DownloadsLeft != db.Unlimited {
			n := f.DownloadsLeft
			item.DownloadsLeft = &n
		}
		resp.Files.Items = append(resp.Files.Items, item)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// stallReader cuts off an upload that makes no progress for timeout. Each
// read pushes the connection's read deadline forward, so a large but steady
// upload can take as long as it needs.
type stallReader struct {
	r       io.Reader
	rc      *http.ResponseController
	timeout time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	s.rc.SetReadDeadline(time.Now().Add(s.timeout)) // ignore ErrNotSupported
	return s.r.Read(p)
}

// ---- download ----

// download serves a share link. Without ?raw it returns a page, which is safe
// for link-preview bots to fetch: it only reads the file's status. The page's
// script then requests ?raw, which streams the file and uses up a download.
// Bots don't run the script, so they never reach ?raw.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	// Go's GET routes also match HEAD. A HEAD must not use up a download.
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		fail(w, http.StatusMethodNotAllowed, "use GET to download")
		return
	}
	raw := false
	for k, vs := range r.URL.Query() {
		if k != "raw" || len(vs) != 1 {
			fail(w, http.StatusBadRequest, "the only allowed query is ?raw")
			return
		}
		raw = true
	}
	if raw {
		s.rawDownload(w, r)
	} else {
		s.downloadPage(w, r)
	}
}

// unavailableStatus maps a "can't download" error to a status code and
// message, or returns 0 for an unexpected error.
func unavailableStatus(err error) (int, string) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return http.StatusNotFound, "no such file"
	case errors.Is(err, db.ErrExpired):
		return http.StatusGone, "this link has expired"
	case errors.Is(err, db.ErrGone):
		return http.StatusGone, "this file has been deleted or has no downloads left"
	case errors.Is(err, db.ErrBusy):
		return http.StatusConflict, "this file is being downloaded right now; try again later"
	default:
		return 0, ""
	}
}

// pageScript starts the real download. "?raw" is relative to the current URL,
// so the file ID never appears in the page. The browser saves the response
// as an attachment and keeps this page open.
const pageScript = `document.getElementById("msg").textContent = "Download started. Check your downloads.";
location.href = "?raw";`

const pageStyle = `body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;line-height:1.5}`

// pageCSP allows only the exact script and style above, by hash.
var pageCSP = func() string {
	hash := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
	}
	return "default-src 'none'; script-src " + hash(pageScript) + "; style-src " + hash(pageStyle) +
		"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

// The page never contains the file name: preview bots read the page, and the
// title and text would end up in the chat app's preview. The Open Graph tags
// give chat apps a generic preview card; error pages get none, so a dead link
// doesn't advertise a download. There's no og:url, which would repeat the
// secret link.
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>File download</title>
{{if .Start}}<meta property="og:type" content="website">
<meta property="og:site_name" content="filedrop">
<meta property="og:title" content="Download file here!">
<meta property="og:description" content="A file was shared with you.">
{{end}}<style>{{.Style}}</style>
</head>
<body>
<p id="msg">{{.Message}}</p>
{{if .Start}}<script>{{.Script}}</script>
<noscript><p>Turn on JavaScript to download, or add ?raw to the end of the link.</p></noscript>{{end}}
</body>
</html>
`))

// downloadPage shows the file's status and, if it's downloadable, starts the
// download. It never takes a slot.
func (s *Server) downloadPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	code, msg, start := http.StatusOK, "Starting your download…", true
	if err := s.store.Check(r.Context(), id); err != nil {
		code, msg = unavailableStatus(err)
		if code == 0 {
			s.log.Error("download check failed", "file", short(id), "err", err)
			code, msg = http.StatusInternalServerError, "could not open file"
		}
		msg = strings.ToUpper(msg[:1]) + msg[1:] + "."
		start = false
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer") // the URL is the secret
	h.Set("X-Robots-Tag", "noindex, nofollow")
	if code == http.StatusConflict {
		h.Set("Retry-After", "60")
	}
	w.WriteHeader(code)
	pageTmpl.Execute(w, struct {
		Message       string
		Start         bool
		Script, Style any
	}{msg, start, template.JS(pageScript), template.CSS(pageStyle)})
}

// rawDownload streams the file, using up one download on success.
func (s *Server) rawDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dl, err := s.store.Open(r.Context(), id)
	if err != nil {
		code, msg := unavailableStatus(err)
		if code == 0 {
			s.log.Error("download open failed", "file", short(id), "err", err)
			code, msg = http.StatusInternalServerError, "could not open file"
		}
		if code == http.StatusConflict {
			w.Header().Set("Retry-After", "60")
		}
		fail(w, code, msg)
		return
	}

	h := w.Header()
	h.Set("Content-Type", dl.File.ContentType)
	h.Set("Content-Length", strconv.FormatInt(dl.Size, 10))
	h.Set("Content-Disposition", contentDisposition(dl.File.OriginalName))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer") // the URL is the secret
	w.WriteHeader(http.StatusOK)

	out := &guardedWriter{
		w: w, rc: http.NewResponseController(w), timeout: s.StallTimeout,
		every: s.RevokeCheck, last: time.Now(),
		check: func() error { return dl.StillAvailable(r.Context()) },
	}
	_, werr := dl.WriteTo(out)
	exhausted, ferr := dl.Finish(r.Context(), werr == nil)
	if ferr != nil {
		s.log.Error("download finish failed", "file", short(id), "err", ferr)
	}
	if werr != nil {
		// Headers are already sent, so the only signal left is cutting the
		// connection short; the client sees fewer bytes than Content-Length.
		s.log.Warn("download interrupted", "file", short(id), "err", werr)
		return
	}
	s.log.Info("file downloaded", "file", short(id), "bytes", dl.Size, "last_download", exhausted)
}

// guardedWriter pushes the write deadline forward on every write (stall
// timeout) and, every `every`, checks the file wasn't revoked.
type guardedWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
	every   time.Duration
	last    time.Time
	check   func() error
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	if time.Since(g.last) >= g.every {
		g.last = time.Now()
		if err := g.check(); err != nil {
			return 0, fmt.Errorf("stopped: %w", err)
		}
	}
	g.rc.SetWriteDeadline(time.Now().Add(g.timeout)) // ignore ErrNotSupported
	return g.w.Write(p)
}

// contentDisposition builds an attachment header. Names that aren't plain
// ASCII get an ASCII fallback plus the RFC 5987 filename* form.
func contentDisposition(name string) string {
	fallback := []rune{}
	ascii := true
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			fallback = append(fallback, '_')
			ascii = false
		} else {
			fallback = append(fallback, r)
		}
	}
	h := `attachment; filename="` + string(fallback) + `"`
	if !ascii {
		h += "; filename*=UTF-8''" + rfc5987Escape(name)
	}
	return h
}

func rfc5987Escape(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && (unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || strings.IndexByte(attrChars, c) >= 0) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---- logging ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real connection for deadlines.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logRequests logs every request by its route pattern, never its path: the
// path of a download is the share link's secret.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "(no route)"
		}
		s.log.Info("request", "method", r.Method, "route", route, "status", sw.status,
			"bytes_out", sw.bytes, "duration", time.Since(start).Round(time.Millisecond).String())
	})
}
