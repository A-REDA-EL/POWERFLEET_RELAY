export type DbMode = "tcp" | "socket"

export interface DbConfig {
  mode: DbMode
  host: string
  port: number
  socket: string
  user: string
  password?: string
  database: string
}

export interface TraccarInfo {
  version: string
  devices: number
  positionsApprox: number
  oldestFix: string | null
  newestFix: string | null
}

export interface Device {
  id: number
  name: string
  uniqueId: string
  status: string | null
  lastUpdate: string | null
  category: string | null
  model: string | null
  disabled: boolean
}

export type JobStatus =
  | "pending"
  | "counting"
  | "running"
  | "paused"
  | "completed"
  | "cancelled"
  | "failed"

/** forward: one request per position in Traccar's format; import: batches to the PowerFleet history import. */
export type JobMode = "forward" | "import"

export interface Job {
  id: number
  name: string
  status: JobStatus
  targetUrl: string
  mode: JobMode
  headers: Record<string, string> | null
  from: string
  to: string
  concurrency: number
  rateLimit: number
  timeoutSeconds: number
  total: number
  sent: number
  rejected: number
  retries: number
  possibleDuplicates: number
  waitingSince: string | null
  lastError: string
  createdAt: string
  startedAt: string | null
  finishedAt: string | null
  devices: number
  active: boolean
}

export interface JobDevice {
  deviceId: number
  name: string
  uniqueId: string
  total: number
  sent: number
  rejected: number
  cursorAt: string | null
  done: boolean
}

export interface JobResponse {
  outcome: "sent" | "rejected"
  status: number
  message: string
  count: number
}

export interface JobEvent {
  at: string
  level: "info" | "warn" | "error"
  message: string
}

export interface JobDetail {
  job: Job
  devices: JobDevice[]
  responses: JobResponse[]
  events: JobEvent[]
}

export interface NewJob {
  name: string
  from: string
  to: string
  deviceIds: number[]
  targetUrl: string
  mode: JobMode
  headers: Record<string, string>
  concurrency: number
  rateLimit: number
  timeoutSeconds: number
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

let onUnauthorized: () => void = () => {}
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers:
      body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    if (res.status === 401 && path !== "/api/login") onUnauthorized()
    throw new ApiError(res.status, data.error ?? res.statusText)
  }
  return data as T
}

export const api = {
  session: () => request<{ authenticated: boolean }>("GET", "/api/session"),
  login: (password: string) => request("POST", "/api/login", { password }),
  logout: () => request("POST", "/api/logout"),
  getDatabase: () =>
    request<{ configured: boolean; config: DbConfig; hasPassword: boolean }>(
      "GET",
      "/api/settings/database"
    ),
  saveDatabase: (cfg: DbConfig) =>
    request("PUT", "/api/settings/database", cfg),
  testDatabase: (cfg: DbConfig) =>
    request<TraccarInfo>("POST", "/api/settings/database/test", cfg),
  info: () => request<TraccarInfo>("GET", "/api/traccar/info"),
  devices: (q: string, limit: number, offset: number) =>
    request<{ items: Device[]; total: number }>(
      "GET",
      `/api/devices?${new URLSearchParams({ q, limit: String(limit), offset: String(offset) })}`
    ),
  deviceIds: (q: string) =>
    request<{ ids: number[]; capped: boolean }>(
      "GET",
      `/api/devices/ids?${new URLSearchParams({ q })}`
    ),
  lookupDevices: (ids: number[]) =>
    request<Device[]>("POST", "/api/devices/lookup", { ids }),
  estimate: (from: string, to: string, deviceIds: number[]) =>
    request<{ total: number; devices: { deviceId: number; count: number }[] }>(
      "POST",
      "/api/estimate",
      {
        from,
        to,
        deviceIds,
      }
    ),
  testTarget: (url: string, mode: JobMode, headers: Record<string, string>) =>
    request<{
      reachable: boolean
      status?: number
      latencyMs?: number
      error?: string
      ready?: boolean
      message?: string
    }>("POST", "/api/target/test", { url, mode, headers }),
  jobs: () => request<Job[]>("GET", "/api/jobs"),
  job: (id: number) => request<JobDetail>("GET", `/api/jobs/${id}`),
  createJob: (job: NewJob) => request<Job>("POST", "/api/jobs", job),
  jobAction: (id: number, action: "pause" | "resume" | "cancel") =>
    request("POST", `/api/jobs/${id}/${action}`),
  deleteJob: (id: number) => request("DELETE", `/api/jobs/${id}`),
}
