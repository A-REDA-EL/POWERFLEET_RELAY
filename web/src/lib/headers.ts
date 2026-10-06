export const API_KEY_HEADER = "X-Api-Key"

export function apiKeyOf(headers: Record<string, string> | null | undefined) {
  const entry = Object.entries(headers ?? {}).find(
    ([k]) => k.toLowerCase() === API_KEY_HEADER.toLowerCase()
  )
  return entry?.[1] ?? ""
}

export function parseHeaders(text: string) {
  const headers: Record<string, string> = {}
  for (const line of text.split("\n")) {
    const i = line.indexOf(":")
    if (i > 0) headers[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return headers
}
