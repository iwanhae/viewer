import { useEffect, useState } from 'react'

export type AdminAvailability = 'unknown' | 'available' | 'absent'

// useAdminAvailable probes whether this deployment serves the admin dashboard
// at all, so the wall can hide the Admin button instead of offering a dead
// link. The server registers the /admin subtree only when it boots with
// ADMIN_TOKEN: enabled deployments answer /admin/api/stats with 401 (fetch
// never pops the Basic-auth dialog — that happens on navigation, which is the
// intended flow for clicking the button), disabled deployments answer with a
// JSON 404 because /admin is never handled by the SPA fallback. Any network
// failure also reads as absent.
export function useAdminAvailable(): AdminAvailability {
  const [availability, setAvailability] = useState<AdminAvailability>('unknown')

  useEffect(() => {
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch('/admin/api/stats', {
          cache: 'no-store',
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        setAvailability(response.status === 404 ? 'absent' : 'available')
      } catch {
        if (controller.signal.aborted) return
        setAvailability('absent')
      }
    })()
    return () => controller.abort()
  }, [])

  return availability
}
