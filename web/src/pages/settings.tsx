import { useEffect, useState } from "react"
import { CheckCircle2Icon, PlugZapIcon, SaveIcon } from "lucide-react"

import { PageHeader } from "@/components/app-shell"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { api, type DbConfig, type DbMode, type TraccarInfo } from "@/lib/api"
import { errorMessage, fmtDateTime, fmtNumber } from "@/lib/format"

const empty: DbConfig = { mode: "socket", host: "", port: 3306, socket: "/run/mysqld/mysqld.sock", user: "", password: "", database: "traccar", timeZone: "" }

export function SettingsPage() {
  const [cfg, setCfg] = useState<DbConfig | null>(null)
  const [hasPassword, setHasPassword] = useState(false)
  const [info, setInfo] = useState<TraccarInfo | null>(null)
  const [testError, setTestError] = useState<string | null>(null)
  const [busy, setBusy] = useState<"test" | "save" | null>(null)

  useEffect(() => {
    api
      .getDatabase()
      .then((res) => {
        setCfg(res.configured ? { ...empty, ...res.config, password: "" } : empty)
        setHasPassword(res.hasPassword)
      })
      .catch((e) => {
        setCfg(empty)
        setTestError(errorMessage(e))
      })
  }, [])

  if (!cfg) {
    return <Skeleton className="h-96 w-full" />
  }
  const set = (patch: Partial<DbConfig>) => setCfg({ ...cfg, ...patch })

  async function test() {
    setBusy("test")
    setTestError(null)
    setInfo(null)
    try {
      setInfo(await api.testDatabase(cfg!))
    } catch (e) {
      setTestError(errorMessage(e))
    } finally {
      setBusy(null)
    }
  }

  async function save() {
    setBusy("save")
    try {
      await api.saveDatabase(cfg!)
      if (cfg!.password) setHasPassword(true)
      toast.add({ title: "Connection saved", type: "success" })
    } catch (e) {
      toast.add({ title: "Could not save", description: errorMessage(e), type: "error" })
    } finally {
      setBusy(null)
    }
  }

  return (
    <>
      <PageHeader title="Traccar database" description="The relay reads devices and position history from here. It only runs SELECT queries." />
      <div className="grid gap-6 lg:grid-cols-[1fr_22rem]">
        <Card>
          <CardHeader>
            <CardTitle>Connection</CardTitle>
            <CardDescription>Use the same credentials Traccar has in /opt/traccar/conf/traccar.xml, or better, a read-only user.</CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <Field>
                <FieldLabel>Connect via</FieldLabel>
                <ToggleGroup
                  variant="outline"
                  value={[cfg.mode]}
                  onValueChange={(v) => v[0] && set({ mode: v[0] as DbMode })}
                >
                  <ToggleGroupItem value="socket">Unix socket</ToggleGroupItem>
                  <ToggleGroupItem value="tcp">TCP host</ToggleGroupItem>
                </ToggleGroup>
              </Field>
              {cfg.mode === "socket" ? (
                <Field>
                  <FieldLabel htmlFor="socket">Socket path (inside the relay container)</FieldLabel>
                  <Input id="socket" value={cfg.socket} onChange={(e) => set({ socket: e.target.value })} />
                  <FieldDescription>The host's MySQL socket directory must be mounted into the container.</FieldDescription>
                </Field>
              ) : (
                <div className="grid grid-cols-[1fr_7rem] gap-3">
                  <Field>
                    <FieldLabel htmlFor="host">Host</FieldLabel>
                    <Input id="host" placeholder="host.docker.internal" value={cfg.host} onChange={(e) => set({ host: e.target.value })} />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="port">Port</FieldLabel>
                    <Input id="port" type="number" value={cfg.port} onChange={(e) => set({ port: Number(e.target.value) })} />
                  </Field>
                </div>
              )}
              <Field>
                <FieldLabel htmlFor="database">Database</FieldLabel>
                <Input id="database" value={cfg.database} onChange={(e) => set({ database: e.target.value })} />
              </Field>
              <Field>
                <FieldLabel htmlFor="timeZone">Time zone of the Traccar server</FieldLabel>
                <Input id="timeZone" placeholder="UTC" value={cfg.timeZone} onChange={(e) => set({ timeZone: e.target.value })} />
                <FieldDescription>
                  Traccar writes times in the time zone of the machine it runs on. Enter that zone (e.g. Europe/Paris; run timedatectl on the
                  Traccar host). Leave empty if it runs in UTC. A wrong value shifts every relayed position by the difference.
                </FieldDescription>
              </Field>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="user">User</FieldLabel>
                  <Input id="user" autoComplete="off" value={cfg.user} onChange={(e) => set({ user: e.target.value })} />
                </Field>
                <Field>
                  <FieldLabel htmlFor="password">Password</FieldLabel>
                  <Input
                    id="password"
                    type="password"
                    autoComplete="new-password"
                    placeholder={hasPassword ? "Unchanged" : ""}
                    value={cfg.password ?? ""}
                    onChange={(e) => set({ password: e.target.value })}
                  />
                </Field>
              </div>
              {testError && (
                <Alert variant="destructive">
                  <AlertTitle>Connection failed</AlertTitle>
                  <AlertDescription>{testError}</AlertDescription>
                </Alert>
              )}
              {info && (
                <Alert>
                  <CheckCircle2Icon />
                  <AlertTitle>Connected to {info.version}</AlertTitle>
                  <AlertDescription>
                    {fmtNumber(info.devices)} devices · ~{fmtNumber(info.positionsApprox)} positions · history from {fmtDateTime(info.oldestFix)} to{" "}
                    {fmtDateTime(info.newestFix)}
                  </AlertDescription>
                </Alert>
              )}
            </FieldGroup>
          </CardContent>
          <CardFooter className="justify-end gap-2">
            <Button variant="outline" onClick={test} disabled={busy !== null}>
              {busy === "test" ? <Spinner data-icon="inline-start" /> : <PlugZapIcon data-icon="inline-start" />}
              Test connection
            </Button>
            <Button onClick={save} disabled={busy !== null}>
              {busy === "save" ? <Spinner data-icon="inline-start" /> : <SaveIcon data-icon="inline-start" />}
              Save
            </Button>
          </CardFooter>
        </Card>

        <Card className="h-fit">
          <CardHeader>
            <CardTitle>MySQL on the host, relay in Docker</CardTitle>
            <CardDescription>Traccar's database runs on the server itself, not in a container. The relay can still reach it.</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4 text-sm">
            <div className="flex flex-col gap-1">
              <p className="font-medium">Unix socket (recommended)</p>
              <p className="text-muted-foreground">
                Mount <code>/run/mysqld</code> into the container. No MySQL network changes. Needs a password user such as <code>traccar@localhost</code>.
              </p>
            </div>
            <div className="flex flex-col gap-1">
              <p className="font-medium">TCP via the Docker host</p>
              <p className="text-muted-foreground">
                Host <code>host.docker.internal</code>. MySQL must listen on the Docker bridge (not only 127.0.0.1) and the user must be allowed from
                172.16.0.0/12.
              </p>
            </div>
          </CardContent>
        </Card>
      </div>
    </>
  )
}
