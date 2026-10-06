const number = new Intl.NumberFormat()

export const fmtNumber = (n: number) => number.format(n)

export function fmtDateTime(iso: string | null | undefined) {
  if (!iso) return "—"
  return new Date(iso).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  })
}

export function fmtRelative(iso: string | null | undefined) {
  if (!iso) return "—"
  const seconds = (Date.now() - new Date(iso).getTime()) / 1000
  if (seconds < 60) return "just now"
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`
  return `${Math.floor(seconds / 86400)} d ago`
}

export function fmtDuration(seconds: number) {
  if (!isFinite(seconds) || seconds < 0) return "—"
  if (seconds < 60) return `${Math.round(seconds)} s`
  if (seconds < 3600)
    return `${Math.floor(seconds / 60)} min ${Math.round(seconds % 60)} s`
  return `${Math.floor(seconds / 3600)} h ${Math.round((seconds % 3600) / 60)} min`
}

export function percent(done: number, total: number) {
  if (total <= 0) return 0
  return Math.min(100, (done / total) * 100)
}

export function hostOf(url: string) {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}

// datetime-local input value (local time) <-> ISO string
export function toLocalInput(d: Date) {
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function errorMessage(e: unknown) {
  return e instanceof Error ? e.message : String(e)
}
