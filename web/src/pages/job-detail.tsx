import { useCallback, useRef, useState } from "react"
import { Link, useNavigate, useParams } from "react-router"
import {
  ArrowLeftIcon,
  CircleAlertIcon,
  CopyIcon,
  PauseIcon,
  PencilIcon,
  PlayIcon,
  Trash2Icon,
  XIcon,
} from "lucide-react"

import { PageHeader } from "@/components/app-shell"
import { EditTargetDialog } from "@/components/edit-target-dialog"
import { StatusBadge } from "@/components/status-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { toast } from "@/components/ui/toast"
import { usePoll } from "@/hooks/use-poll"
import { api } from "@/lib/api"
import {
  errorMessage,
  fmtDateTime,
  fmtDuration,
  fmtNumber,
  fmtRelative,
  percent,
} from "@/lib/format"

export function JobDetailPage() {
  const id = Number(useParams().id)
  const navigate = useNavigate()
  const [active, setActive] = useState(true)
  const [rate, setRate] = useState(0)
  const [editing, setEditing] = useState(false)
  const last = useRef<{ t: number; done: number } | null>(null)

  // Fetches the job and derives a smoothed delivery rate from successive polls.
  const load = useCallback(async () => {
    const detail = await api.job(id)
    const now = Date.now()
    const done = detail.job.sent + detail.job.rejected
    const prev = last.current
    last.current = { t: now, done }
    setActive(detail.job.active)
    if (!detail.job.active) setRate(0)
    else if (prev && now > prev.t) {
      const instant = ((done - prev.done) * 1000) / (now - prev.t)
      setRate((r) => (r === 0 ? instant : r * 0.6 + instant * 0.4))
    }
    return { ...detail, fetchedAt: now }
  }, [id])
  const { data, error, refresh } = usePoll(load, active ? 1000 : 5000, [id])
  const job = data?.job

  async function action(a: "pause" | "resume" | "cancel") {
    try {
      await api.jobAction(id, a)
      await refresh()
    } catch (e) {
      toast.add({
        title: `Could not ${a}`,
        description: errorMessage(e),
        type: "error",
      })
    }
  }

  async function remove() {
    try {
      await api.deleteJob(id)
      navigate("/jobs")
    } catch (e) {
      toast.add({
        title: "Could not delete",
        description: errorMessage(e),
        type: "error",
      })
    }
  }

  if (error && !data) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{error}</AlertDescription>
      </Alert>
    )
  }
  if (!data || !job) return <Skeleton className="h-96 w-full" />

  const done = job.sent + job.rejected
  const remaining = Math.max(0, job.total - done)
  const finished = job.status === "completed" || job.status === "cancelled"
  const elapsed = job.startedAt
    ? ((job.finishedAt ? new Date(job.finishedAt).getTime() : data.fetchedAt) -
        new Date(job.startedAt).getTime()) /
      1000
    : 0
  const devicesDone = data.devices.filter((d) => d.done).length
  const notStored = data.responses.filter(
    (r) =>
      r.outcome === "sent" &&
      !/inserted|success|ok/i.test(r.message) &&
      r.message !== ""
  )

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="mb-2"
        nativeButton={false}
        render={<Link to="/jobs" />}
      >
        <ArrowLeftIcon data-icon="inline-start" />
        History
      </Button>
      <PageHeader
        title={job.name}
        description={`${fmtDateTime(job.from)} → ${fmtDateTime(job.to)} · ${job.mode === "import" ? "PowerFleet import → " : ""}${job.targetUrl}`}
        actions={
          <>
            <StatusBadge job={job} />
            {job.active ? (
              <Button variant="outline" onClick={() => action("pause")}>
                <PauseIcon data-icon="inline-start" />
                Pause
              </Button>
            ) : (
              (job.status === "paused" || job.status === "failed") && (
                <Button onClick={() => action("resume")}>
                  <PlayIcon data-icon="inline-start" />
                  Resume
                </Button>
              )
            )}
            {!finished && (
              <ConfirmButton
                label="Cancel"
                icon={<XIcon data-icon="inline-start" />}
                title="Cancel this relay?"
                description="Positions already delivered stay on the target. A cancelled relay cannot be resumed."
                confirm="Cancel relay"
                onConfirm={() => action("cancel")}
              />
            )}
            {!job.active && !finished && (
              <Button variant="outline" onClick={() => setEditing(true)}>
                <PencilIcon data-icon="inline-start" />
                Edit target
              </Button>
            )}
            <Button
              variant="outline"
              onClick={() => navigate("/new", { state: { from: job } })}
            >
              <CopyIcon data-icon="inline-start" />
              Run again
            </Button>
            {!job.active && (
              <ConfirmButton
                label=""
                icon={<Trash2Icon />}
                title="Delete this relay from history?"
                description="Only the history entry is removed. Nothing changes on the target."
                confirm="Delete"
                onConfirm={remove}
              />
            )}
          </>
        }
      />

      {editing && (
        <EditTargetDialog
          job={job}
          open
          onOpenChange={setEditing}
          onSaved={refresh}
        />
      )}

      {job.waitingSince && (
        <Alert variant="destructive" className="mb-6">
          <CircleAlertIcon />
          <AlertTitle>
            Waiting since {fmtRelative(job.waitingSince)} — retrying
            automatically
          </AlertTitle>
          <AlertDescription>{job.lastError}</AlertDescription>
        </Alert>
      )}
      {job.status === "failed" && job.lastError && !job.waitingSince && (
        <Alert variant="destructive" className="mb-6">
          <CircleAlertIcon />
          <AlertTitle>Failed</AlertTitle>
          <AlertDescription>{job.lastError}</AlertDescription>
        </Alert>
      )}
      {notStored.length > 0 && (
        <Alert className="mb-6">
          <CircleAlertIcon />
          <AlertTitle>
            The target accepted some positions without storing them
          </AlertTitle>
          <AlertDescription>
            {notStored
              .map((r) => `${fmtNumber(r.count)} × "${r.message}"`)
              .join(" · ")}{" "}
            — check the Responses tab.
          </AlertDescription>
        </Alert>
      )}

      <Card className="mb-6">
        <CardContent className="flex flex-col gap-6">
          <div className="flex flex-col gap-2">
            <div className="flex items-baseline justify-between text-sm">
              <span className="text-2xl font-semibold tabular-nums">
                {percent(done, job.total).toFixed(1)}%
              </span>
              <span className="text-muted-foreground tabular-nums">
                {fmtNumber(done)} of {job.total ? fmtNumber(job.total) : "…"}{" "}
                positions · {devicesDone}/{job.devices} devices done
              </span>
            </div>
            <Progress value={percent(done, job.total)} />
          </div>
          <dl className="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
            <Stat label="Delivered" value={fmtNumber(job.sent)} />
            <Stat label="Rejected (4xx)" value={fmtNumber(job.rejected)} />
            <Stat label="Retries" value={fmtNumber(job.retries)} />
            <Stat
              label="Possible duplicates"
              value={fmtNumber(job.possibleDuplicates)}
              hint="Upper bound on extra copies the target may have stored: attempts that timed out or lost the connection, then were sent again"
            />
            <Stat
              label="Rate"
              value={job.active ? `${fmtNumber(Math.round(rate))}/s` : "—"}
            />
            <Stat
              label={job.active ? "Time left" : "Elapsed"}
              value={
                job.active
                  ? rate > 0
                    ? fmtDuration(remaining / rate)
                    : "—"
                  : fmtDuration(elapsed)
              }
            />
          </dl>
        </CardContent>
      </Card>

      <Tabs defaultValue="devices">
        <TabsList>
          <TabsTrigger value="devices">Devices</TabsTrigger>
          <TabsTrigger value="responses">Responses</TabsTrigger>
          <TabsTrigger value="activity">Activity</TabsTrigger>
        </TabsList>
        <TabsContent value="devices">
          <Card className="py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Device</TableHead>
                  <TableHead className="w-56">Progress</TableHead>
                  <TableHead className="text-right">Delivered</TableHead>
                  <TableHead className="text-right">Rejected</TableHead>
                  <TableHead>Up to fix time</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.devices.map((d) => (
                  <TableRow key={d.deviceId}>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{d.name}</span>
                        <span className="font-mono text-xs text-muted-foreground">
                          {d.uniqueId}
                        </span>
                        {d.done && <Badge variant="secondary">Done</Badge>}
                      </div>
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <Progress
                          value={
                            d.total > 0
                              ? percent(d.sent + d.rejected, d.total)
                              : d.done
                                ? 100
                                : 0
                          }
                          className="flex-1"
                        />
                        <span className="w-24 text-right text-xs text-muted-foreground tabular-nums">
                          {fmtNumber(d.sent + d.rejected)}/
                          {d.total >= 0 ? fmtNumber(d.total) : "…"}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtNumber(d.sent)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmtNumber(d.rejected)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {fmtDateTime(d.cursorAt)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        </TabsContent>
        <TabsContent value="responses">
          <Card>
            <CardHeader>
              <CardTitle>What the target answered</CardTitle>
              <CardDescription>
                Grouped by outcome, HTTP status and the "message" field of the
                response body.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Outcome</TableHead>
                    <TableHead>HTTP</TableHead>
                    <TableHead>Message</TableHead>
                    <TableHead className="text-right">Count</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.responses.map((r) => (
                    <TableRow key={`${r.outcome}-${r.status}-${r.message}`}>
                      <TableCell>
                        <Badge
                          variant={
                            r.outcome === "sent" ? "secondary" : "destructive"
                          }
                        >
                          {r.outcome === "sent" ? "Accepted" : "Rejected"}
                        </Badge>
                      </TableCell>
                      <TableCell className="tabular-nums">{r.status}</TableCell>
                      <TableCell className="max-w-md truncate">
                        {r.message || "—"}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {fmtNumber(r.count)}
                      </TableCell>
                    </TableRow>
                  ))}
                  {data.responses.length === 0 && (
                    <TableRow>
                      <TableCell
                        colSpan={4}
                        className="h-16 text-center text-muted-foreground"
                      >
                        No responses yet.
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </CardContent>
          </Card>
        </TabsContent>
        <TabsContent value="activity">
          <Card>
            <CardContent>
              <ol className="flex flex-col gap-3">
                {data.events.map((e, i) => (
                  <li key={i} className="flex gap-3 text-sm">
                    <span className="w-40 shrink-0 text-muted-foreground tabular-nums">
                      {fmtDateTime(e.at)}
                    </span>
                    <Badge
                      variant={
                        e.level === "error"
                          ? "destructive"
                          : e.level === "warn"
                            ? "outline"
                            : "secondary"
                      }
                    >
                      {e.level}
                    </Badge>
                    <span className="min-w-0 break-words">{e.message}</span>
                  </li>
                ))}
              </ol>
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>
    </>
  )
}

function Stat({
  label,
  value,
  hint,
}: {
  label: string
  value: string
  hint?: string
}) {
  return (
    <div className="flex flex-col gap-1" title={hint}>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-lg font-semibold tabular-nums">{value}</dd>
    </div>
  )
}

function ConfirmButton(props: {
  label: string
  icon: React.ReactNode
  title: string
  description: string
  confirm: string
  onConfirm: () => void
}) {
  return (
    <AlertDialog>
      <AlertDialogTrigger
        render={
          <Button
            variant="outline"
            size={props.label ? "default" : "icon"}
            aria-label={props.label || props.confirm}
          >
            {props.icon}
            {props.label}
          </Button>
        }
      />
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{props.title}</AlertDialogTitle>
          <AlertDialogDescription>{props.description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Keep</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={props.onConfirm}>
            {props.confirm}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
