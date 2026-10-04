import { useEffect, useState } from "react"
import { BrowserRouter, Navigate, Route, Routes } from "react-router"

import { AppShell } from "@/components/app-shell"
import { Spinner } from "@/components/ui/spinner"
import { api, setUnauthorizedHandler } from "@/lib/api"
import { JobDetailPage } from "@/pages/job-detail"
import { JobsPage } from "@/pages/jobs"
import { LoginPage } from "@/pages/login"
import { NewRelayPage } from "@/pages/new-relay"
import { SettingsPage } from "@/pages/settings"

export function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)

  useEffect(() => {
    setUnauthorizedHandler(() => setAuthed(false))
    api
      .session()
      .then((s) => setAuthed(s.authenticated))
      .catch(() => setAuthed(false))
  }, [])

  if (authed === null) {
    return (
      <div className="flex min-h-svh items-center justify-center">
        <Spinner />
      </div>
    )
  }
  if (!authed) return <LoginPage onSignedIn={() => setAuthed(true)} />

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<AppShell onSignOut={() => setAuthed(false)} />}>
          <Route path="/" element={<Navigate to="/jobs" replace />} />
          <Route path="/jobs" element={<JobsPage />} />
          <Route path="/jobs/:id" element={<JobDetailPage />} />
          <Route path="/new" element={<NewRelayPage />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/jobs" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  )
}

export default App
