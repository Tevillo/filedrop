# filedrop

A small self-hosted service for sharing files through expiring links. Senders
upload with an API key and get back a share link; anyone with the link can
download the file until it expires or runs out of downloads. Files are
encrypted as they arrive and never stored unencrypted.

> **Status:** early development. Uploads, downloads, expiry, download limits
> and cleanup work, and the service can be deployed behind Caddy and systemd.
> There is no browser frontend yet.

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Uploading and downloading](#uploading-and-downloading)
- [Administration](#administration)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Development](#development)
- [Limitations](#limitations)

## How it works

```
sender ──PUT + API key──▶ reverse proxy (HTTPS) ──▶ filedrop ──▶ SQLite + encrypted files
recipient ──share link──▶ reverse proxy (HTTPS) ──▶ filedrop ──▶ decrypt and stream
```

- **One program.** `filedrop` is a single binary: `filedrop serve` runs the
  HTTP server and the cleaner; the other subcommands manage keys.
- **Encryption.** Each file gets its own random data key and is encrypted as it
  streams in (AES-256-GCM, [DARE](https://github.com/minio/sio) format). The
  data key is itself encrypted ("wrapped") with a **master key** and stored in
  the database. Keep the master key on a different drive from the data: a copy
  of the data folder alone can't be decrypted.
- **Metadata** (API keys, file records, limits) lives in a SQLite database.
- **Share links** contain a random 128-bit file ID; the link is the only thing
  a recipient needs.
- **API keys** are stored only as SHA-256 hashes.
- **Cleanup.** A cleaner inside `serve` periodically deletes files that have
  expired, used up their downloads, or belong to a revoked key.

## Quick start

Requires Go 1.27 or newer. Everything here runs locally with the default
settings (files under `~/.local/share/filedrop/`).

```sh
go build -o filedrop ./cmd/filedrop

./filedrop masterkey init                    # once; never overwrites an existing key
KEY=$(./filedrop keys create --name laptop)  # the key is shown only once
./filedrop serve                             # listens on 127.0.0.1:8090
```

In another terminal:

```sh
LINK=$(curl -fsS -T notes.txt -H "Authorization: Bearer $KEY" http://127.0.0.1:8090/filedrop/)
curl -OJ "$LINK?raw"
```

## Uploading and downloading

Examples use `https://example.com/filedrop`; replace it with your
`FILEDROP_PUBLIC_URL` (locally, `http://127.0.0.1:8090/filedrop`).

### Upload

```sh
curl -T "holiday photo.jpg" \
     -H "Authorization: Bearer $KEY" \
     --url-query expires=2h --url-query downloads=3 \
     https://example.com/filedrop/
```

The response is the share link, on one line, so `LINK=$(curl ...)` captures it:

```
https://example.com/filedrop/lLwqZ6_kBjVaTSUo0ww8mA
```

- `-T FILE` sends the file as the request body. Because the URL ends in `/`,
  curl adds the file name to it (`.../filedrop/holiday%20photo.jpg`), and that
  name is what the recipient's download is saved as.
- **Use `--url-query` for settings, not `?` in the URL.** curl only adds the
  file name when the URL ends in `/` with nothing after it; with
  `.../filedrop/?expires=2h` the name is dropped and the server answers `404`.
  (Alternatively, write the name yourself:
  `.../filedrop/holiday%20photo.jpg?expires=2h`.)
- The upload must have a `Content-Length`, so send a file, not a pipe.

| Setting | Values | Default |
| --- | --- | --- |
| `expires` | Go duration from `1s` to `48h`, e.g. `30m`, `2h` | `48h` |
| `downloads` | `-1` (unlimited) or at least `1` | `1` |

Any other setting name is rejected, so a typo like `expire=2h` fails instead of
silently using the default.

### Download

Anyone with the link can download; no key is needed.

**In a browser**, open the link. A small page loads and its script starts the
download right away; there's no button to click. If the file can't be
downloaded (expired, used up, busy), the page says why instead. The page never
shows the file name.

**With curl**, add `?raw` to get the file itself:

```sh
curl -OJ "https://example.com/filedrop/lLwqZ6_kBjVaTSUo0ww8mA?raw"
```

`-O -J` saves the file under its original name. Without `?raw`, curl gets the
HTML page. Any other query (`?Raw`, `?raw&x=1`) is rejected with `400`.

- Each successful download uses one of the file's downloads; a download that
  fails partway doesn't.
- While all remaining downloads are in progress at once, further requests get
  `409`.
- Loading the page never uses a download; only `?raw` does.
- `HEAD` requests are refused.
- A transfer (upload or download) that makes no progress for 60 seconds is cut
  off. A cut-off upload leaves nothing behind.

**Link previews:** chat apps (Discord, Slack, iMessage…) fetch pasted links to
build a preview. They get the page, which uses nothing up, and they don't run
its script, so they never reach `?raw`. A working link shows a generic card
("Download file here!" / "A file was shared with you.") with no file details;
a dead link shows no card. Link scanners that *do* run JavaScript
(some email and antivirus scanners) can still use up a download; allow more
than one download if a link goes through one.

### Status

See your key's files and the service's storage:

```sh
curl -sS -H "Authorization: Bearer $KEY" https://example.com/filedrop/api/status
```

```json
{
  "storage": {
    "used_bytes": 8568,
    "service_limit_bytes": 10737418240,
    "service_remaining_bytes": 10737409672
  },
  "files": {
    "count": 2,
    "items": [
      {"size_bytes": 2856, "expires_in_seconds": 3540, "downloads_left": 1},
      {"size_bytes": 5712, "expires_in_seconds": 172000, "downloads_left": null}
    ]
  }
}
```

- Lists only files uploaded with **your** key that can still be downloaded:
  not expired, not used up. Soonest-expiring first.
- Entries are anonymous on purpose: no link, ID or name.
- `downloads_left: null` means unlimited.
- `used_bytes` is the total of the listed files. `service_remaining_bytes` is
  what a new upload can use, across all keys; it also counts uploads in
  progress and used-up files until the next cleanup sweep (at most
  `FILEDROP_SWEEP_INTERVAL`).

### Responses

| Status | Meaning |
| --- | --- |
| `201` | Uploaded; the body is the share link |
| `200` | Download page, the file itself with `?raw`, or status JSON |
| `400` | Invalid setting, file name or download query, or the upload didn't complete |
| `401` | Missing, unknown or revoked API key |
| `404` | No such file (or no file name in the upload URL) |
| `405` | Wrong method, e.g. `HEAD` on a link |
| `409` | All of the file's remaining downloads are in progress; try again later |
| `410` | Expired, used up, or deleted |
| `411` | No `Content-Length`; upload a file, not a pipe |
| `413` | Larger than `FILEDROP_MAX_FILE_BYTES` |
| `507` | Total storage (`FILEDROP_MAX_TOTAL_BYTES`) is full |

Errors are a single line of text starting with `error:`, except on the download
page, which shows them as a sentence in the page.

## Administration

All commands read their settings from the environment; see
[Loading `.env`](#loading-env).

| Command | What it does |
| --- | --- |
| `filedrop masterkey init` | Create the master key (refuses to overwrite) |
| `filedrop keys create --name NAME` | Create an API key and print it once |
| `filedrop keys list` | List keys: ID, name, creation time, revoked status (never the keys themselves) |
| `filedrop keys revoke NAME` | Revoke a key and delete its files |
| `filedrop serve` | Run the HTTP server and the cleaner until Ctrl-C or SIGTERM |

### Master key

`filedrop masterkey init` writes 32 random bytes to `FILEDROP_MASTER_KEY` with
permissions `0400` (readable only by its owner). It **never overwrites** an
existing key. Losing or replacing the master key makes every stored file
unreadable, so back it up somewhere separate from the database and the
uploaded files.

### API keys

```sh
filedrop keys create --name laptop
```

The key starts with `fd_` and is printed **once**, as the only thing on
standard output. Only its hash is saved, so it can't be shown again; if it's
lost, create a new one. Each key needs a unique name of at most 64 characters.

```sh
filedrop keys revoke laptop
```

Revoking is permanent. The key stops working and every file it uploaded
becomes undownloadable immediately; downloads already in progress are cut off
within about 10 seconds, and the files are removed from disk on the cleaner's
next sweep.

### Running the service

`filedrop serve` runs the HTTP server on `FILEDROP_LISTEN` together with the
cleaner. On Ctrl-C or SIGTERM it stops accepting requests and gives transfers
in progress up to 30 seconds to finish.

**Cleaner.** On startup it releases download slots left over from a previous
run and abandons unfinished uploads, before the HTTP server accepts requests.
It then sweeps immediately and every `FILEDROP_SWEEP_INTERVAL`. Each sweep
marks files that have expired or used up their downloads, and that nobody is
downloading, as deleted (freeing their storage), then removes the encrypted
data of every deleted file from disk. A file is never removed mid-download.

### Logs

Logs are plain `key=value` lines on standard error at Info level (under
systemd, see `journalctl -u filedrop`). They record each request's method,
route, status, size and duration; uploads (short file ID, key name, size,
limits); completed downloads; cleaner actions; and errors.

They never record full file IDs (the secret part of a share link), API keys,
file names, or client addresses. Requests are logged by route
(`GET /filedrop/{id}`), and file IDs are cut to 6 characters. A reverse proxy
with access logging enabled will still record full URLs.

## Configuration

Settings come from environment variables, normally kept in a `.env` file:

```sh
cp .env.example .env
```

| Variable | What it sets | Default | Flag override |
| --- | --- | --- | --- |
| `FILEDROP_DB` | SQLite database file | `~/.local/share/filedrop/filedrop.db` | `--db PATH` |
| `FILEDROP_MASTER_KEY` | Master key file | `~/.local/share/filedrop/master.key` | `--master-key PATH` |
| `FILEDROP_DATA_DIR` | Directory for encrypted files | `~/.local/share/filedrop/files` | none |
| `FILEDROP_MAX_FILE_BYTES` | Largest single upload, in bytes | `2147483648` (2 GiB) | none |
| `FILEDROP_MAX_TOTAL_BYTES` | Total size of all stored files, in bytes | `10737418240` (10 GiB) | none |
| `FILEDROP_SWEEP_INTERVAL` | How often the cleaner runs (Go duration, at least `1s`) | `5m` | none |
| `FILEDROP_LISTEN` | Address the HTTP server listens on | `127.0.0.1:8090` | none |
| `FILEDROP_PUBLIC_URL` | Start of every share link (no trailing slash) | `http://<FILEDROP_LISTEN>/filedrop` | none |

A flag beats the environment variable, which beats the default. Directories are
created automatically. `.env` is git-ignored; never commit it.

### Loading `.env`

Go doesn't read `.env` by itself; something has to turn it into environment
variables.

- **systemd:** the unit loads it with `EnvironmentFile=`.
- **A shell:** export it before running commands, so the CLI uses the same
  database and key as the service:

  ```sh
  set -a; . ./.env; set +a
  ./filedrop keys list
  ```

  `set -a` matters: a plain `. ./.env` sets the variables without exporting
  them, and the CLI silently falls back to the default paths.

### Database

The database and its tables are created automatically the first time any
command opens it; there is no separate setup step. Table definitions live in
`internal/db/migrations/` as numbered SQL files built into the binary. SQLite
records the last one applied (`PRAGMA user_version`), and any newer ones are
applied in order when the database is opened, so upgrading the binary also
upgrades the schema.

```sh
sqlite3 "$FILEDROP_DB" '.tables'             # api_keys  files
sqlite3 "$FILEDROP_DB" '.schema files'       # full table definition
sqlite3 "$FILEDROP_DB" 'PRAGMA user_version' # last migration applied
```

## Deployment

This section deploys filedrop on a Linux server with systemd, behind
[Caddy](https://caddyserver.com/) for HTTPS. filedrop listens only on
`127.0.0.1`, so the reverse proxy is the only way in. Replace these
placeholders throughout:

| Placeholder | Meaning |
| --- | --- |
| `example.com` | Your domain |
| `user@server` | SSH login for the server |
| `/mnt/keys` | A drive **other than** the one holding `/srv`, for the master key |

Layout:

| What | Where | Owner, mode |
| --- | --- | --- |
| Program | `/usr/local/bin/filedrop` | root, `0755` (the service can run it but not change it) |
| Settings | `/srv/filedrop/.env` | filedrop, `0600` |
| Database | `/srv/filedrop/filedrop.db` | filedrop (inside a `0700` folder) |
| Encrypted files | `/srv/filedrop/files/` | filedrop, `0700` |
| Master key | `/mnt/keys/filedrop/master.key` | filedrop, `0400` (folder `0700`) |

Deployment has two stages: run the service by hand and test it from outside,
then hand it to systemd. If something fails in the first stage, the cause is
the proxy or permissions, not systemd.

### 1. Build and copy

On your own machine:

```sh
scripts/build-linux.sh                 # dist/filedrop for linux/amd64
scp dist/filedrop deploy/filedrop.service .env.example user@server:/tmp/
```

The build has no C dependencies, so it runs on any Linux server. For an ARM
server, run `GOARCH=arm64 scripts/build-linux.sh`.

### 2. Create the user and folders

On the server:

```sh
sudo useradd --system --home-dir /srv/filedrop --shell /usr/sbin/nologin filedrop
sudo install -d -o filedrop -g filedrop -m 0700 /srv/filedrop
sudo install -d -o filedrop -g filedrop -m 0700 /mnt/keys/filedrop
sudo install -o root -g root -m 0755 /tmp/filedrop /usr/local/bin/filedrop
```

The `filedrop` user must be able to pass through every folder above its key
folder. Check with `sudo -u filedrop ls /mnt/keys/filedrop`; "Permission
denied" means a parent folder lacks the `x` permission for other users.

### 3. Settings

Edit `/tmp/.env.example` so these lines read:

```sh
FILEDROP_DB=/srv/filedrop/filedrop.db
FILEDROP_MASTER_KEY=/mnt/keys/filedrop/master.key
FILEDROP_DATA_DIR=/srv/filedrop/files
FILEDROP_PUBLIC_URL=https://example.com/filedrop
```

then install it:

```sh
sudo install -o filedrop -g filedrop -m 0600 /tmp/.env.example /srv/filedrop/.env
```

### 4. Create the keys as the filedrop user

Run every command on the server as `filedrop` with the settings loaded, the
same way systemd will, so permission problems show up now. This shell function
does that (add it to your shell config to keep it):

```sh
fdrop() {
  sudo -u filedrop sh -c 'cd /srv/filedrop && set -a && . ./.env && set +a && exec /usr/local/bin/filedrop "$@"' filedrop "$@"
}
```

```sh
fdrop masterkey init
fdrop keys create --name laptop     # prints the key once
```

Back up the master key somewhere other than the server's data drive.

### 5. Add the Caddy route

In the Caddyfile (often `/etc/caddy/Caddyfile`), route `/filedrop/*` to
filedrop inside your site's block, as in
[`deploy/Caddyfile.snippet`](deploy/Caddyfile.snippet):

```
example.com {
	handle /filedrop/* {
		reverse_proxy 127.0.0.1:8090
	}

	# Anything the site already served goes in its own handle.
	handle {
		reverse_proxy 127.0.0.1:3000
	}
}
```

Caddy picks one `handle` block per request, the most specific match. Use
`handle`, not `handle_path`: `handle_path` strips `/filedrop` from the path
and every request would get `404`. Then:

```sh
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

### 6. Run by hand and test from outside

On the server (in `tmux` or a spare terminal):

```sh
fdrop serve
```

From a device on a **different network**, so requests go through DNS, HTTPS
and the proxy like a real user's:

```sh
KEY=fd_...                               # from step 4
head -c 300M /dev/urandom > big.bin
sha256sum big.bin                        # macOS: shasum -a 256

# Upload: prints an https://example.com/filedrop/... link
LINK=$(curl -fsS -T big.bin -H "Authorization: Bearer $KEY" https://example.com/filedrop/)
echo "$LINK"

# Download: the hash must match the one above
curl -fsS -o got.bin "$LINK?raw" && sha256sum got.bin

# Second download of a 1-download file: expect 410
curl -s -o /dev/null -w '%{http_code}\n' "$LINK?raw"

# Wrong key: expect 401
echo hi > small.txt
curl -s -o /dev/null -w '%{http_code}\n' -T small.txt -H "Authorization: Bearer fd_wrong" https://example.com/filedrop/
```

Stop `fdrop serve` with Ctrl-C when done.

### 7. Hand it to systemd

Edit `/tmp/filedrop.service` so `RequiresMountsFor=` names your key folder,
then:

```sh
sudo cp /tmp/filedrop.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now filedrop
systemctl status filedrop
journalctl -u filedrop -f                # logs
```

Run the test from step 6 again. `RequiresMountsFor=` makes the service wait
for the key drive to be mounted, so it still finds its key after a reboot.

### Updating

```sh
scripts/build-linux.sh && scp dist/filedrop user@server:/tmp/      # your machine
sudo install -o root -g root -m 0755 /tmp/filedrop /usr/local/bin/filedrop \
  && sudo systemctl restart filedrop                                # server
```

A restart stops transfers in progress (after up to 30 seconds); their download
slots are released when the service starts again.

## Development

```sh
go vet ./...
go test ./...
```

To try commands without touching your real files, point them at a throwaway
folder:

```sh
export FILEDROP_DB=/tmp/filedrop-test/filedrop.db
export FILEDROP_MASTER_KEY=/tmp/filedrop-test/master.key
export FILEDROP_DATA_DIR=/tmp/filedrop-test/files
```

## Limitations

- **No browser frontend yet.** It will need its own way to authenticate
  senders, since an API key embedded in a web page is visible to anyone using
  it.
- **Server-side encryption, not end-to-end.** Anyone with a shell on the server
  as the `filedrop` user (or root) can read the master key and decrypt files.
- **Link scanners that run JavaScript can use up downloads** (see [Download](#download)).
