import { Badge } from "@/components/ui/badge"
import { Spinner } from "@/components/ui/spinner"
import type { Job } from "@/lib/api"

const labels: Record<Job["status"], string> = {
  pending: "Pending",
  counting: "Counting",
  running: "Running",
  paused: "Paused",
  completed: "Completed",
  cancelled: "Cancelled",
  failed: "Failed",
}

export function StatusBadge({ job }: { job: Pick<Job, "status" | "waitingSince"> }) {
  if ((job.status === "running" || job.status === "counting") && job.waitingSince) {
    return (
      <Badge variant="destructive">
        <Spinner data-icon="inline-start" />
        Waiting for target
      </Badge>
    )
  }
  switch (job.status) {
    case "running":
    case "counting":
    case "pending":
      return (
        <Badge>
          <Spinner data-icon="inline-start" />
          {labels[job.status]}
        </Badge>
      )
    case "completed":
      return <Badge variant="secondary">{labels[job.status]}</Badge>
    case "failed":
      return <Badge variant="destructive">{labels[job.status]}</Badge>
    default:
      return <Badge variant="outline">{labels[job.status]}</Badge>
  }
}
