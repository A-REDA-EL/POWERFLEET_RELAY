# PowerFleet Relay

Replays position history from a Traccar database to any HTTP server, in exactly the JSON format
Traccar's `forward.type=json` forwarder posts (`{"position": …, "device": …}`).

Use it to:
- re-send what a server missed while it or its database was down;
- seed a new server/database with the history of selected devices.

Web UI: pick a period (24 h … 3 months, or custom), select devices (or all), give a target URL,
review the position count, start. Every relay is kept in the history with its progress, what the
target answered, and an activity log.

## Target types

**PowerFleet import** (recommended for PowerFleet Servers): batches of up to 200 positions to
`POST /api/server/import` with the Server's `X-Api-Key`. The Server writes history only (no
alerts, notifications or missions), computes odometer/engine hours exactly like its live path,
and skips positions it already has, so a job can be repeated or overlap existing data safely.
Needs `GPS_IMPORT_API_KEY` set on the Server (see its `docs/history-import.md`). In the
simulation it filled a 63,661-position gap in 15 s and a repeated run inserted nothing.

- 401 (wrong key) or 404 "History import is disabled" fails the job at once.
- 404 "Unknown object" (vehicle missing on the target) rejects that batch; the job continues.
- 413 halves the batch size; 5xx and network errors are retried.

**Traccar forward**: one request per position, exactly like Traccar's `forward.url`, for any
server. Goes through the target's live processing (alerts may fire) and is not deduplicated by
the PowerFleet Server, so only replay ranges the target is missing.

## How delivery works

- Each device's positions are read with keyset paging on `(fixtime, id)` (index
  `position_deviceid_fixtime`) and sent **one at a time, in order**. Devices run in parallel.
- **Retries never give up**: network errors, timeouts, HTTP 408/425/429 and 5xx are retried with
  backoff (0.5 s → 30 s) until the target accepts. The job shows *Waiting for target* meanwhile.
- Other 4xx answers are counted as **rejected** and the job moves on.
- Every position is checkpointed in SQLite as soon as the target answered, and marked *in flight*
  just before it is sent. Pause/resume, `docker compose down/up` and redeploys continue exactly
  where they stopped; a pause or graceful stop waits for the in-flight request, so nothing is
  sent twice.
- **Possible duplicates** are counted, not hidden: a position resent after an ambiguous failure
  (timeout, dropped connection, HTTP 500/504) may already have been stored by the target. After a
  hard kill (`kill -9`, OOM, power loss of the container) at most the one in-flight position per
  device is resent, and it is counted.
- Responses are grouped by status and body `message`, so a target that answers 2xx without storing
  (the PowerFleet Server's `"Unknown object"`, `"Invalid speed"`, …) is visible.

The guarantee only holds if the target answers non-2xx when it fails to store a position.

### Surviving restarts and power cuts

- A job that was running when the relay stopped (container restart, host reboot, power loss) is
  **continued automatically** at the next start, after the last acknowledged batch of each device.
  It does not matter if the Traccar database is not up yet: the relay waits for it, also while
  counting positions. The checkpoint database is written with `synchronous=FULL`.
- The host must start Docker at boot (`systemctl enable docker`); the compose file sets
  `restart: unless-stopped`.
- A plain-text HTTP 404 (a reverse proxy while the Server restarts) is retried. Only the Server's
  own JSON errors fail a job: wrong API key (401/403), import disabled or wrong path (404 JSON).
  Fix the target (**Edit target**) and press **Continue**.
- In the history, a paused, failed **or cancelled** job can be **Continued**. **Retry rejected**
  sends the devices that had rejections again from the start of their range (the Server skips
  what it already stored): use it after registering missing vehicles. **Continue to now** opens a
  new relay for the same devices that starts where the old range ended.

## Reaching Traccar's database (MySQL on the host, relay in Docker)

Traccar's MySQL/MariaDB runs on the server itself. The container can reach it two ways:

**Unix socket (recommended).** `docker-compose.yml` mounts the host's socket directory
`/run/mysqld` (set `MYSQL_SOCKET_DIR` if yours differs) read-only. No MySQL network changes. In the
UI choose *Unix socket*, path `/run/mysqld/mysqld.sock`. Use a password user that is allowed from
`localhost` (users that authenticate by OS socket, like Debian's `root`, cannot be used from a
container).

**TCP.** Host `host.docker.internal`, port 3306. MySQL must listen on the Docker bridge
(`bind-address` not only `127.0.0.1`) and the user must be allowed from `172.16.0.0/12`.

Create a read-only user (it only needs these two tables):

```sql
CREATE USER 'relay_ro'@'localhost' IDENTIFIED BY '…';      -- socket
CREATE USER 'relay_ro'@'172.%' IDENTIFIED BY '…';          -- TCP from containers
GRANT SELECT ON traccar.tc_devices   TO 'relay_ro'@'localhost', 'relay_ro'@'172.%';
GRANT SELECT ON traccar.tc_positions TO 'relay_ro'@'localhost', 'relay_ro'@'172.%';
```

Timestamps are read with the session time zone pinned to UTC, so the MySQL server's own time
zone does not matter.

## Run

```bash
cp .env.example .env   # set RELAY_ADMIN_PASSWORD and RELAY_SECRET_KEY (openssl rand -hex 32)
docker compose up -d --build
# UI on http://SERVER_IP:8090 (plain HTTP: firewall it or put a proxy with TLS in front)
```

| Variable | |
|---|---|
| `RELAY_ADMIN_PASSWORD` | UI password (required, ≥ 8 chars) |
| `RELAY_SECRET_KEY` | encrypts the stored DB password and signs sessions (required; changing it makes the stored password unreadable) |
| `RELAY_DB_*` | optional: pre-configure the connection on first start (`MODE`, `SOCKET`, `HOST`, `PORT`, `USER`, `PASSWORD`, `NAME`, `TIMEZONE`) |
| `RELAY_DB_TIMEZONE` | time zone of the machine Traccar runs on (e.g. `Europe/Paris`; `timedatectl` shows it). Traccar writes position times in that zone; leave empty only if it runs in UTC, otherwise every relayed position is shifted |
| `RELAY_IMPORT_BATCH` / `RELAY_IMPORT_MAX_KB` | PowerFleet import: positions per request (default 200, max 1000) and largest request body in KB (default 512, max 900). Bigger batches mean fewer round trips |
| `MYSQL_SOCKET_DIR` | host socket directory to mount (default `/run/mysqld`) |

State (settings, jobs, checkpoints) lives in the `relay_data` volume (`/data/relay.db`).

For targets on another Docker network (e.g. the PowerFleet Server's), attach the relay to that
network and use the service name in the target URL.

## Development

```bash
cd web && npm install && npm run dev        # UI on :5173, proxies /api to :8090
RELAY_ADMIN_PASSWORD=devpassword RELAY_SECRET_KEY=dev-secret-key-123 RELAY_DATA_DIR=./data go run ./cmd/relay
go test ./...
```

Go 1.26, React + Vite + shadcn/ui (Base UI), SQLite (pure Go), single static binary with the UI
embedded; the image is distroless and runs as non-root.
