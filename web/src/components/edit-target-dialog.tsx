import { useState } from "react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { api, type Job } from "@/lib/api"
import { errorMessage } from "@/lib/format"
import { API_KEY_HEADER, apiKeyOf, parseHeaders } from "@/lib/headers"

// Fixes where a stopped job delivers (wrong URL or API key) without losing its progress.
export function EditTargetDialog({
  job,
  open,
  onOpenChange,
  onSaved,
}: {
  job: Job
  open: boolean
  onOpenChange: (open: boolean) => void
  onSaved: () => void
}) {
  const isImport = job.mode === "import"
  const [targetUrl, setTargetUrl] = useState(job.targetUrl)
  const [apiKey, setApiKey] = useState(() => apiKeyOf(job.headers))
  const [headersText, setHeadersText] = useState(() =>
    Object.entries(job.headers ?? {})
      .filter(([k]) => k.toLowerCase() !== API_KEY_HEADER.toLowerCase())
      .map(([k, v]) => `${k}: ${v}`)
      .join("\n")
  )
  const [saving, setSaving] = useState(false)

  async function save() {
    setSaving(true)
    try {
      const headers = parseHeaders(headersText)
      if (isImport) headers[API_KEY_HEADER] = apiKey.trim()
      await api.updateJob(job.id, {
        targetUrl: targetUrl.trim(),
        headers,
        concurrency: job.concurrency,
        rateLimit: job.rateLimit,
        timeoutSeconds: job.timeoutSeconds,
      })
      toast.add({ title: "Target updated", type: "success" })
      onOpenChange(false)
      onSaved()
    } catch (e) {
      toast.add({
        title: "Could not save",
        description: errorMessage(e),
        type: "error",
      })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit target</DialogTitle>
          <DialogDescription>
            Progress is kept. Press Resume afterwards to continue from where it
            stopped.
          </DialogDescription>
        </DialogHeader>
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="edit-url">Target URL</FieldLabel>
            <Input
              id="edit-url"
              value={targetUrl}
              onChange={(e) => setTargetUrl(e.target.value)}
            />
          </Field>
          {isImport && (
            <Field>
              <FieldLabel htmlFor="edit-key">API key (X-Api-Key)</FieldLabel>
              <Input
                id="edit-key"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
              />
            </Field>
          )}
          <Field>
            <FieldLabel htmlFor="edit-headers">
              {isImport ? "Other headers" : "Headers"} (one per line, Name:
              value)
            </FieldLabel>
            <Textarea
              id="edit-headers"
              rows={3}
              value={headersText}
              onChange={(e) => setHeadersText(e.target.value)}
            />
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            disabled={saving || (isImport && apiKey.trim() === "")}
            onClick={save}
          >
            {saving && <Spinner data-icon="inline-start" />}
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
