import { useEffect, useMemo, useState } from "react"
import { Link, useLocation, useNavigate } from "react-router"
import {
  ArrowLeftIcon,
  ArrowRightIcon,
  CalendarRangeIcon,
  CheckIcon,
  DatabaseIcon,
  PlayIcon,
  SearchIcon,
  SendIcon,
  TriangleAlertIcon,
  WifiIcon,
} from "lucide-react"

import { PageHeader } from "@/components/app-shell"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { api, type Device, type Job, type JobMode } from "@/lib/api"
import {
  errorMessage,
  fmtDateTime,
  fmtDuration,
  fmtNumber,
  fmtRelative,
  toLocalInput,
} from "@/lib/format"
import { cn } from "@/lib/utils"

const presets = [
  { id: "1d", label: "24 hours", days: 1 },
  { id: "7d", label: "7 days", days: 7 },
  { id: "30d", label: "1 month", days: 30 },
  { id: "60d", label: "2 months", days: 60 },
  { id: "90d", label: "3 months", days: 90 },
  { id: "custom", label: "Custom", days: 0 },
] as const

const API_KEY_HEADER = "X-Api-Key"

function apiKeyOf(headers: Record<string, string> | null | undefined) {
  const entry = Object.entries(headers ?? {}).find(
    ([k]) => k.toLowerCase() === API_KEY_HEADER.toLowerCase()
  )
  return entry?.[1] ?? ""
}

const steps = [
  { title: "Range", icon: CalendarRangeIcon },
  { title: "Devices", icon: DatabaseIcon },
  { title: "Target", icon: SendIcon },
  { title: "Review", icon: CheckIcon },
]

function presetToRange(days: number) {
  const to = new Date()
  return { from: new Date(to.getTime() - days * 86400e3), to }
}

function isHttpUrl(value: string) {
  try {
    const u = new URL(value)
    return u.protocol === "http:" || u.protocol === "https:"
  } catch {
    return false
  }
}

function parseHeaders(text: string) {
  const headers: Record<string, string> = {}
  for (const line of text.split("\n")) {
    const i = line.indexOf(":")
    if (i > 0) headers[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return headers
}

export function NewRelayPage() {
  const navigate = useNavigate()
  const prefill = (useLocation().state as { from?: Job } | null)?.from

  const [step, setStep] = useState(0)
  const [preset, setPreset] = useState<string>(prefill ? "custom" : "30d")
  // preset ranges end "now" as of when the preset was picked
  const [presetRange, setPresetRange] = useState(() => presetToRange(30))
  const [customFrom, setCustomFrom] = useState(() =>
    toLocalInput(prefill ? new Date(prefill.from) : presetToRange(30).from)
  )
  const [customTo, setCustomTo] = useState(() =>
    toLocalInput(prefill ? new Date(prefill.to) : new Date())
  )

  const [devices, setDevices] = useState<Device[] | null>(null)
  const [devicesError, setDevicesError] = useState<string | null>(null)
  const [search, setSearch] = useState("")
  const [selected, setSelected] = useState<Set<number>>(new Set())

  const [mode, setMode] = useState<JobMode>(prefill?.mode ?? "forward")
  const [targetUrl, setTargetUrl] = useState(prefill?.targetUrl ?? "")
  const [apiKey, setApiKey] = useState(() => apiKeyOf(prefill?.headers))
  const [headersText, setHeadersText] = useState(
    prefill?.headers
      ? Object.entries(prefill.headers)
          .filter(([k]) => k.toLowerCase() !== API_KEY_HEADER.toLowerCase())
          .map(([k, v]) => `${k}: ${v}`)
          .join("\n")
      : ""
  )
  const [concurrency, setConcurrency] = useState(prefill?.concurrency ?? 4)
  const [rateLimit, setRateLimit] = useState(prefill?.rateLimit ?? 100)
  const [timeoutSeconds, setTimeoutSeconds] = useState(
    prefill?.timeoutSeconds ?? 20
  )
  const [targetCheck, setTargetCheck] = useState<{
    ok: boolean
    text: string
  } | null>(null)
  const [checking, setChecking] = useState(false)

  const [name, setName] = useState("")
  const [estimate, setEstimate] = useState<{
    total: number
    byDevice: Map<number, number>
  } | null>(null)
  const [estimating, setEstimating] = useState(false)
  const [starting, setStarting] = useState(false)

  const range = useMemo(
    () =>
      preset === "custom"
        ? { from: new Date(customFrom), to: new Date(customTo) }
        : presetRange,
    [preset, presetRange, customFrom, customTo]
  )

  function choosePreset(id: string) {
    setPreset(id)
    const p = presets.find((x) => x.id === id)
    if (p && p.days > 0) setPresetRange(presetToRange(p.days))
  }
  const rangeValid =
    !isNaN(range.from.getTime()) &&
    !isNaN(range.to.getTime()) &&
    range.from < range.to

  useEffect(() => {
    api
      .devices()
      .then((list) => {
        setDevices(list)
        if (prefill) {
          // "Run again" keeps the same device selection when the job's devices are known
          api
            .job(prefill.id)
            .then((d) => setSelected(new Set(d.devices.map((x) => x.deviceId))))
        }
      })
      .catch((e) => setDevicesError(errorMessage(e)))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!devices) return []
    if (!q) return devices
    return devices.filter((d) =>
      [d.name, d.uniqueId, d.category, d.model].some((v) =>
        v?.toLowerCase().includes(q)
      )
    )
  }, [devices, search])
  const allFilteredSelected =
    filtered.length > 0 && filtered.every((d) => selected.has(d.id))
  const someFilteredSelected = filtered.some((d) => selected.has(d.id))

  function toggleAll(checked: boolean) {
    const next = new Set(selected)
    for (const d of filtered) {
      if (checked) next.add(d.id)
      else next.delete(d.id)
    }
    setSelected(next)
  }

  function toggle(id: number, checked: boolean) {
    const next = new Set(selected)
    if (checked) next.add(id)
    else next.delete(id)
    setSelected(next)
  }

  const targetValid =
    isHttpUrl(targetUrl) && (mode === "forward" || apiKey.trim() !== "")

  function targetHeaders() {
    const headers = parseHeaders(headersText)
    if (mode === "import") headers[API_KEY_HEADER] = apiKey.trim()
    return headers
  }

  function chooseMode(next: JobMode) {
    setMode(next)
    setTargetCheck(null)
    // the Server imports a couple of batches at a time and throttles itself
    if (next === "import") {
      setConcurrency(2)
      setRateLimit(0)
    } else {
      setConcurrency(4)
      setRateLimit(100)
    }
  }

  async function checkTarget() {
    setChecking(true)
    try {
      const r = await api.testTarget(targetUrl, mode, targetHeaders())
      if (!r.reachable)
        setTargetCheck({ ok: false, text: r.error ?? "Unreachable" })
      else if (mode === "import")
        setTargetCheck({
          ok: !!r.ready,
          text: `${r.message} (${r.latencyMs} ms)`,
        })
      else
        setTargetCheck({
          ok: true,
          text: `Reachable (HTTP ${r.status}, ${r.latencyMs} ms)`,
        })
    } catch (e) {
      setTargetCheck({ ok: false, text: errorMessage(e) })
    } finally {
      setChecking(false)
    }
  }

  async function runEstimate() {
    setEstimating(true)
    setEstimate(null)
    try {
      const r = await api.estimate(
        range.from.toISOString(),
        range.to.toISOString(),
        [...selected]
      )
      setEstimate({
        total: r.total,
        byDevice: new Map(r.devices.map((d) => [d.deviceId, d.count])),
      })
    } catch (e) {
      toast.add({
        title: "Could not count positions",
        description: errorMessage(e),
        type: "error",
      })
    } finally {
      setEstimating(false)
    }
  }

  function goTo(next: number) {
    setStep(next)
    if (next === 3) runEstimate()
  }

  async function start() {
    setStarting(true)
    try {
      const job = await api.createJob({
        name,
        from: range.from.toISOString(),
        to: range.to.toISOString(),
        deviceIds: [...selected],
        targetUrl,
        mode,
        headers: targetHeaders(),
        concurrency,
        rateLimit,
        timeoutSeconds,
      })
      toast.add({
        title: "Relay started",
        description: job.name,
        type: "success",
      })
      navigate(`/jobs/${job.id}`)
    } catch (e) {
      toast.add({
        title: "Could not start relay",
        description: errorMessage(e),
        type: "error",
      })
      setStarting(false)
    }
  }

  const canNext = [rangeValid, selected.size > 0, targetValid, true][step]

  if (devicesError) {
    return (
      <>
        <PageHeader title="New relay" />
        <Empty className="border bg-background">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <DatabaseIcon />
            </EmptyMedia>
            <EmptyTitle>Cannot read the Traccar database</EmptyTitle>
            <EmptyDescription>{devicesError}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button nativeButton={false} render={<Link to="/settings" />}>
              Configure the database
            </Button>
          </EmptyContent>
        </Empty>
      </>
    )
  }

  const selectedDevices = devices?.filter((d) => selected.has(d.id)) ?? []
  const etaSeconds =
    estimate && rateLimit > 0 ? estimate.total / rateLimit : NaN

  return (
    <>
      <PageHeader
        title="New relay"
        description="Send Traccar position history to a server, exactly like Traccar's JSON forwarder does."
      />

      <ol className="mb-6 grid grid-cols-4 gap-2">
        {steps.map((s, i) => (
          <li key={s.title}>
            <button
              type="button"
              disabled={i > step}
              onClick={() => goTo(i)}
              className={cn(
                "flex w-full items-center gap-2 rounded-lg border bg-background px-3 py-2 text-left text-sm transition-colors disabled:cursor-default",
                i === step && "border-primary ring-1 ring-primary",
                i > step && "text-muted-foreground"
              )}
            >
              <span
                className={cn(
                  "flex size-6 shrink-0 items-center justify-center rounded-full border text-xs",
                  i < step &&
                    "border-primary bg-primary text-primary-foreground"
                )}
              >
                {i < step ? <CheckIcon className="size-3.5" /> : i + 1}
              </span>
              <span className="truncate font-medium">{s.title}</span>
            </button>
          </li>
        ))}
      </ol>

      {step === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Which period?</CardTitle>
            <CardDescription>
              Positions are selected by their GPS fix time.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <Field>
                <FieldLabel>Range</FieldLabel>
                <ToggleGroup
                  variant="outline"
                  value={[preset]}
                  onValueChange={(v) => v[0] && choosePreset(v[0])}
                  className="flex-wrap"
                >
                  {presets.map((p) => (
                    <ToggleGroupItem key={p.id} value={p.id}>
                      {p.label}
                    </ToggleGroupItem>
                  ))}
                </ToggleGroup>
              </Field>
              {preset === "custom" && (
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field>
                    <FieldLabel htmlFor="from">From</FieldLabel>
                    <Input
                      id="from"
                      type="datetime-local"
                      value={customFrom}
                      onChange={(e) => setCustomFrom(e.target.value)}
                    />
                  </Field>
                  <Field data-invalid={!rangeValid || undefined}>
                    <FieldLabel htmlFor="to">To</FieldLabel>
                    <Input
                      id="to"
                      type="datetime-local"
                      aria-invalid={!rangeValid}
                      value={customTo}
                      onChange={(e) => setCustomTo(e.target.value)}
                    />
                    {!rangeValid && (
                      <FieldDescription>
                        "To" must be after "From".
                      </FieldDescription>
                    )}
                  </Field>
                </div>
              )}
              {rangeValid && (
                <Alert>
                  <CalendarRangeIcon />
                  <AlertTitle>
                    {fmtDateTime(range.from.toISOString())} →{" "}
                    {fmtDateTime(range.to.toISOString())}
                  </AlertTitle>
                  <AlertDescription>
                    Times are shown in your browser's time zone and sent to the
                    server in UTC.
                  </AlertDescription>
                </Alert>
              )}
            </FieldGroup>
          </CardContent>
        </Card>
      )}

      {step === 1 && (
        <Card>
          <CardHeader>
            <CardTitle>Which devices?</CardTitle>
            <CardDescription>
              {devices
                ? `${fmtNumber(selected.size)} of ${fmtNumber(devices.length)} devices selected`
                : "Loading devices from Traccar…"}
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <InputGroup>
              <InputGroupAddon>
                <SearchIcon />
              </InputGroupAddon>
              <InputGroupInput
                placeholder="Search name, IMEI, category…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
              />
            </InputGroup>
            {!devices ? (
              <div className="flex flex-col gap-2">
                {Array.from({ length: 6 }).map((_, i) => (
                  <Skeleton key={i} className="h-9 w-full" />
                ))}
              </div>
            ) : (
              <ScrollArea className="h-[26rem] rounded-lg border">
                <Table>
                  <TableHeader className="sticky top-0 bg-background">
                    <TableRow>
                      <TableHead className="w-10">
                        <Checkbox
                          aria-label="Select all"
                          checked={allFilteredSelected}
                          indeterminate={
                            !allFilteredSelected && someFilteredSelected
                          }
                          onCheckedChange={(c) => toggleAll(Boolean(c))}
                        />
                      </TableHead>
                      <TableHead>Name</TableHead>
                      <TableHead>IMEI / unique ID</TableHead>
                      <TableHead>Category</TableHead>
                      <TableHead>Last update</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {filtered.map((d) => (
                      <TableRow
                        key={d.id}
                        data-state={selected.has(d.id) ? "selected" : undefined}
                        className="cursor-pointer"
                        onClick={() => toggle(d.id, !selected.has(d.id))}
                      >
                        <TableCell onClick={(e) => e.stopPropagation()}>
                          <Checkbox
                            aria-label={`Select ${d.name}`}
                            checked={selected.has(d.id)}
                            onCheckedChange={(c) => toggle(d.id, Boolean(c))}
                          />
                        </TableCell>
                        <TableCell className="font-medium">
                          <div className="flex items-center gap-2">
                            {d.name}
                            {d.disabled && (
                              <Badge variant="outline">Disabled</Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {d.uniqueId}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {d.category ?? "—"}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {fmtRelative(d.lastUpdate)}
                        </TableCell>
                      </TableRow>
                    ))}
                    {filtered.length === 0 && (
                      <TableRow>
                        <TableCell
                          colSpan={5}
                          className="h-24 text-center text-muted-foreground"
                        >
                          No devices match "{search}".
                        </TableCell>
                      </TableRow>
                    )}
                  </TableBody>
                </Table>
              </ScrollArea>
            )}
          </CardContent>
        </Card>
      )}

      {step === 2 && (
        <Card>
          <CardHeader>
            <CardTitle>Where should the data go?</CardTitle>
            <CardDescription>
              {mode === "import"
                ? "Batches go to the PowerFleet Server's history import: history only, no alerts, and positions it already has are skipped."
                : "Each position is POSTed as JSON in Traccar's forward format, exactly like Traccar's forward.url."}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <Field>
                <FieldLabel>Target type</FieldLabel>
                <ToggleGroup
                  variant="outline"
                  value={[mode]}
                  onValueChange={(v) => v[0] && chooseMode(v[0] as JobMode)}
                >
                  <ToggleGroupItem value="import">
                    PowerFleet import
                  </ToggleGroupItem>
                  <ToggleGroupItem value="forward">
                    Traccar forward
                  </ToggleGroupItem>
                </ToggleGroup>
                <FieldDescription>
                  {mode === "import"
                    ? "Use this to fill gaps or seed a PowerFleet Server: safe to repeat, never triggers alerts."
                    : "Any HTTP server that accepts Traccar's JSON forward; goes through the target's live processing."}
                </FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor="target">Target URL</FieldLabel>
                <InputGroup>
                  <InputGroupInput
                    id="target"
                    placeholder={
                      mode === "import"
                        ? "http://powerfleet-server:3005/api/server/import"
                        : "http://powerfleet-server:3005/api/server"
                    }
                    value={targetUrl}
                    onChange={(e) => {
                      setTargetUrl(e.target.value)
                      setTargetCheck(null)
                    }}
                  />
                  <InputGroupAddon align="inline-end">
                    <InputGroupButton
                      onClick={checkTarget}
                      disabled={!targetValid || checking}
                    >
                      {checking ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <WifiIcon data-icon="inline-start" />
                      )}
                      Check
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                {targetCheck && (
                  <FieldDescription
                    className={cn(!targetCheck.ok && "text-destructive")}
                  >
                    {targetCheck.text}
                  </FieldDescription>
                )}
                {!targetCheck && (
                  <FieldDescription>
                    {mode === "import"
                      ? "Check sends an empty batch to verify the URL and API key."
                      : "Check only opens a connection; no position is sent."}
                  </FieldDescription>
                )}
              </Field>
              {mode === "import" && (
                <Field>
                  <FieldLabel htmlFor="api-key">API key</FieldLabel>
                  <Input
                    id="api-key"
                    type="password"
                    autoComplete="off"
                    value={apiKey}
                    onChange={(e) => {
                      setApiKey(e.target.value)
                      setTargetCheck(null)
                    }}
                  />
                  <FieldDescription>
                    The Server's GPS_IMPORT_API_KEY, sent as the{" "}
                    {API_KEY_HEADER} header.
                  </FieldDescription>
                </Field>
              )}
              <Field>
                <FieldLabel htmlFor="headers">Extra headers</FieldLabel>
                <Textarea
                  id="headers"
                  rows={3}
                  className="font-mono text-xs"
                  placeholder={"Authorization: Bearer …"}
                  value={headersText}
                  onChange={(e) => setHeadersText(e.target.value)}
                />
                <FieldDescription>
                  One per line, "Name: value". Content-Type is always
                  application/json.
                </FieldDescription>
              </Field>
              <Separator />
              <div className="grid gap-3 sm:grid-cols-3">
                <Field>
                  <FieldLabel htmlFor="rate">Max positions / second</FieldLabel>
                  <Input
                    id="rate"
                    type="number"
                    min={0}
                    value={rateLimit}
                    onChange={(e) => setRateLimit(Number(e.target.value))}
                  />
                  <FieldDescription>
                    Protects the target. 0 = unlimited.
                  </FieldDescription>
                </Field>
                <Field>
                  <FieldLabel htmlFor="concurrency">
                    Devices in parallel
                  </FieldLabel>
                  <Input
                    id="concurrency"
                    type="number"
                    min={1}
                    max={64}
                    value={concurrency}
                    onChange={(e) => setConcurrency(Number(e.target.value))}
                  />
                  <FieldDescription>
                    Each device is always sent in order.
                  </FieldDescription>
                </Field>
                <Field>
                  <FieldLabel htmlFor="timeout">Request timeout (s)</FieldLabel>
                  <Input
                    id="timeout"
                    type="number"
                    min={1}
                    value={timeoutSeconds}
                    onChange={(e) => setTimeoutSeconds(Number(e.target.value))}
                  />
                </Field>
              </div>
            </FieldGroup>
          </CardContent>
        </Card>
      )}

      {step === 3 && (
        <div className="grid gap-6 lg:grid-cols-[1fr_24rem]">
          <Card>
            <CardHeader>
              <CardTitle>Review</CardTitle>
              <CardDescription>
                Nothing is sent until you start the relay.
              </CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-6">
              <Field>
                <FieldLabel htmlFor="name">Name</FieldLabel>
                <Input
                  id="name"
                  placeholder={`Relay ${range.from.toISOString().slice(0, 10)} → ${range.to.toISOString().slice(0, 10)}`}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
              <dl className="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
                <Summary label="Positions">
                  {estimating || !estimate ? (
                    <Spinner />
                  ) : (
                    fmtNumber(estimate.total)
                  )}
                </Summary>
                <Summary label="Devices">{fmtNumber(selected.size)}</Summary>
                <Summary label="Target type">
                  {mode === "import" ? "PowerFleet import" : "Traccar forward"}
                </Summary>
                <Summary label="Rate">
                  {rateLimit > 0 ? `${fmtNumber(rateLimit)}/s` : "Unlimited"}
                </Summary>
                <Summary label="Duration at full rate">
                  {estimate ? fmtDuration(etaSeconds) : "—"}
                </Summary>
              </dl>
              <div className="flex flex-col gap-1 text-sm">
                <span className="text-muted-foreground">Range</span>
                <span>
                  {fmtDateTime(range.from.toISOString())} →{" "}
                  {fmtDateTime(range.to.toISOString())}
                </span>
              </div>
              <div className="flex flex-col gap-1 text-sm">
                <span className="text-muted-foreground">Target</span>
                <span className="font-mono text-xs break-all">{targetUrl}</span>
              </div>
              <ScrollArea className="h-56 rounded-lg border">
                <Table>
                  <TableHeader className="sticky top-0 bg-background">
                    <TableRow>
                      <TableHead>Device</TableHead>
                      <TableHead className="text-right">Positions</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {selectedDevices.map((d) => (
                      <TableRow key={d.id}>
                        <TableCell>
                          {d.name}{" "}
                          <span className="font-mono text-xs text-muted-foreground">
                            {d.uniqueId}
                          </span>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {estimate ? (
                            fmtNumber(estimate.byDevice.get(d.id) ?? 0)
                          ) : (
                            <Skeleton className="ml-auto h-4 w-12" />
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </ScrollArea>
            </CardContent>
          </Card>
          <div className="flex flex-col gap-4">
            {mode === "import" ? (
              <>
                <Alert>
                  <CheckIcon />
                  <AlertTitle>History only, safe to repeat</AlertTitle>
                  <AlertDescription>
                    Positions go to the vehicles' history with the same odometer
                    and engine hours the live path computes. No alerts,
                    notifications or missions run, and positions the Server
                    already has are skipped.
                  </AlertDescription>
                </Alert>
                <Alert>
                  <TriangleAlertIcon />
                  <AlertTitle>Vehicles must exist on the target</AlertTitle>
                  <AlertDescription>
                    Positions of vehicles the Server doesn't know are counted as
                    rejected ("Unknown object").
                  </AlertDescription>
                </Alert>
              </>
            ) : (
              <>
                <Alert>
                  <TriangleAlertIcon />
                  <AlertTitle>
                    Replaying into a server that already has this data
                    duplicates it
                  </AlertTitle>
                  <AlertDescription>
                    Live endpoints such as /api/server don't skip points they
                    already stored. To fill gaps on a PowerFleet Server, use
                    PowerFleet import instead.
                  </AlertDescription>
                </Alert>
                <Alert>
                  <TriangleAlertIcon />
                  <AlertTitle>
                    The target must answer non-2xx when it fails to store a
                    point
                  </AlertTitle>
                  <AlertDescription>
                    The relay retries every network error, timeout and 5xx until
                    it succeeds. A 2xx answer is treated as stored, so watch the
                    response breakdown (e.g. "Unknown object" for vehicles
                    missing on the target).
                  </AlertDescription>
                </Alert>
                <Alert>
                  <TriangleAlertIcon />
                  <AlertTitle>Alerts may fire on the target</AlertTitle>
                  <AlertDescription>
                    Points newer than a vehicle's last known position go through
                    the target's normal event processing.
                  </AlertDescription>
                </Alert>
              </>
            )}
          </div>
        </div>
      )}

      <div className="mt-6 flex justify-between">
        <Button
          variant="outline"
          onClick={() => goTo(step - 1)}
          disabled={step === 0}
        >
          <ArrowLeftIcon data-icon="inline-start" />
          Back
        </Button>
        {step < 3 ? (
          <Button onClick={() => goTo(step + 1)} disabled={!canNext}>
            Next
            <ArrowRightIcon data-icon="inline-end" />
          </Button>
        ) : (
          <Button
            onClick={start}
            disabled={
              starting || estimating || !estimate || estimate.total === 0
            }
          >
            {starting ? (
              <Spinner data-icon="inline-start" />
            ) : (
              <PlayIcon data-icon="inline-start" />
            )}
            {estimate?.total === 0
              ? "No positions in this range"
              : "Start relay"}
          </Button>
        )}
      </div>
    </>
  )
}

function Summary({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1 rounded-lg border p-3">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-lg font-semibold tabular-nums">{children}</dd>
    </div>
  )
}
