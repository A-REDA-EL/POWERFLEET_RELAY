import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react"

// Loads data and refreshes it every `interval` ms (pass 0 to stop polling).
export function usePoll<T>(load: () => Promise<T>, interval: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const loadRef = useRef(load)
  useLayoutEffect(() => {
    loadRef.current = load
  })

  const refresh = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    // first load right away, then on every tick; state updates happen in the async callback
    const first = setTimeout(refresh, 0)
    const id = interval ? setInterval(refresh, interval) : undefined
    return () => {
      clearTimeout(first)
      clearInterval(id)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [interval, refresh, ...deps])

  return { data, error, refresh }
}
