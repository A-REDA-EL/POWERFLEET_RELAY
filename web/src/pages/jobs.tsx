import { Link, useNavigate } from "react-router"
import { HistoryIcon, PlusIcon } from "lucide-react"

import { PageHeader } from "@/components/app-shell"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
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
import { usePoll } from "@/hooks/use-poll"
import { api } from "@/lib/api"
import {
  fmtDateTime,
  fmtNumber,
  fmtRelative,
  hostOf,
  percent,
} from "@/lib/format"

export function JobsPage() {
  const navigate = useNavigate()
  const { data: jobs, error } = usePoll(api.jobs, 3000)

  const newButton = (
    <Button nativeButton={false} render={<Link to="/new" />}>
      <PlusIcon data-icon="inline-start" />
      New relay
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Relay history"
        description="Every replay job, its progress and outcome."
        actions={newButton}
      />
      {error && (
        <Alert variant="destructive" className="mb-4">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {!jobs ? (
        <Skeleton className="h-64 w-full" />
      ) : jobs.length === 0 ? (
        <Empty className="border bg-background">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <HistoryIcon />
            </EmptyMedia>
            <EmptyTitle>No relays yet</EmptyTitle>
            <EmptyDescription>
              Pick a period and devices, then send their history to a server.
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>{newButton}</EmptyContent>
        </Empty>
      ) : (
        <Card className="py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-48">Progress</TableHead>
                <TableHead>Range</TableHead>
                <TableHead>Target</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {jobs.map((j) => {
                const done = j.sent + j.rejected
                return (
                  <TableRow
                    key={j.id}
                    className="cursor-pointer"
                    onClick={() => navigate(`/jobs/${j.id}`)}
                  >
                    <TableCell>
                      <div className="flex flex-col">
                        <span className="font-medium">{j.name}</span>
                        <span className="text-xs text-muted-foreground">
                          {fmtNumber(j.devices)}{" "}
                          {j.devices === 1 ? "device" : "devices"}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge job={j} />
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-col gap-1">
                        <Progress value={percent(done, j.total)} />
                        <span className="text-xs text-muted-foreground tabular-nums">
                          {fmtNumber(done)} /{" "}
                          {j.total ? fmtNumber(j.total) : "…"}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {fmtDateTime(j.from)}
                      <br />
                      {fmtDateTime(j.to)}
                    </TableCell>
                    <TableCell className="max-w-48 truncate font-mono text-xs">
                      {j.mode === "import" && (
                        <Badge variant="secondary" className="mr-1.5 font-sans">
                          Import
                        </Badge>
                      )}
                      {hostOf(j.targetUrl)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {fmtRelative(j.createdAt)}
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      )}
    </>
  )
}
