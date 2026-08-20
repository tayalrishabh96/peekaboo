// Thin wrapper around the backend REST API.

async function req(path, opts) {
  const res = await fetch(path, opts)
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body.error) msg = body.error
    } catch {
      /* ignore parse errors */
    }
    throw new Error(msg)
  }
  if (res.status === 204) return null
  return res.json()
}

export const api = {
  getConfig: () => req('/api/config'),
  listContexts: () => req('/api/contexts'),
  listNamespaces: (context) =>
    req(`/api/namespaces?context=${encodeURIComponent(context)}`),
  listServices: (context, namespace) =>
    req(
      `/api/services?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`,
    ),
  createLink: (payload) =>
    req('/api/links', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  listForwards: () => req('/api/forwards'),
  startForward: (payload) =>
    req('/api/forwards', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  stopForward: (id) => req(`/api/forwards/${id}`, { method: 'DELETE' }),
}
