import { useEffect, useState } from 'react'
import { api } from './api'

// Namespaces the token is always expected to have access to. Shown for every
// cluster so they can be selected even when listing namespaces is forbidden.
const DEFAULT_NAMESPACES = ['devtroncd', 'monitoring']

// Reusable selection column: shows a title, a list, a loading/error/empty state.
function Column({ title, items, selected, onSelect, loading, error, render, filterKey }) {
  const [q, setQ] = useState('')
  const filtered = items.filter((it) =>
    filterKey(it).toLowerCase().includes(q.toLowerCase()),
  )
  return (
    <div className="column">
      <div className="column-head">
        <h2>{title}</h2>
        {items.length > 0 && (
          <input
            className="filter"
            placeholder="Filter…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        )}
      </div>
      <div className="list">
        {loading && <div className="hint">Loading…</div>}
        {error && <div className="error">{error}</div>}
        {!loading && !error && items.length === 0 && (
          <div className="hint">Nothing here.</div>
        )}
        {!loading &&
          !error &&
          filtered.map((it) => {
            const key = filterKey(it)
            return (
              <button
                key={key}
                className={selected === key ? 'row selected' : 'row'}
                onClick={() => onSelect(it)}
              >
                {render(it)}
              </button>
            )
          })}
      </div>
    </div>
  )
}

// NamespaceColumn is like Column but supports manual namespace entry, which is
// required when the token can't list namespaces cluster-wide (View-in-namespace
// roles). It also downgrades a 403 to a friendly hint instead of a red error.
function NamespaceColumn({
  namespaces,
  selected,
  onSelect,
  onAdd,
  loading,
  error,
  forbidden,
  hasContext,
}) {
  const [name, setName] = useState('')

  function submit(e) {
    e.preventDefault()
    onAdd(name)
    setName('')
  }

  return (
    <div className="column">
      <div className="column-head">
        <h2>Namespaces</h2>
      </div>
      {hasContext && (
        <form className="ns-add" onSubmit={submit}>
          <input
            className="filter"
            placeholder="Type a namespace…"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <button type="submit" className="ns-add-btn" disabled={!name.trim()}>
            Add
          </button>
        </form>
      )}
      <div className="list">
        {loading && <div className="hint">Loading…</div>}
        {!loading && !hasContext && <div className="hint">Select a cluster.</div>}
        {!loading && forbidden && (
          <div className="hint">
            This token can’t list namespaces cluster-wide. Type a namespace you
            have access to (e.g. <code>monitoring</code>, <code>devtroncd</code>)
            above.
          </div>
        )}
        {!loading && error && !forbidden && <div className="error">{error}</div>}
        {!loading &&
          namespaces.map((n) => (
            <button
              key={n.name}
              className={selected === n.name ? 'row selected' : 'row'}
              onClick={() => onSelect(n.name)}
            >
              <span className="row-title">{n.name}</span>
            </button>
          ))}
      </div>
    </div>
  )
}

export default function Browse({ onForwardStarted, config }) {
  const proxyMode = config?.forwardMode === 'proxy'
  const [contexts, setContexts] = useState([])
  const [namespaces, setNamespaces] = useState([])
  const [services, setServices] = useState([])

  const [context, setContext] = useState(null)
  const [namespace, setNamespace] = useState(null)
  const [service, setService] = useState(null)

  const [loading, setLoading] = useState({ ctx: false, ns: false, svc: false })
  const [errors, setErrors] = useState({})
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState(null)

  // Load contexts on mount.
  useEffect(() => {
    setLoading((l) => ({ ...l, ctx: true }))
    api
      .listContexts()
      .then(setContexts)
      .catch((e) => setErrors((x) => ({ ...x, ctx: e.message })))
      .finally(() => setLoading((l) => ({ ...l, ctx: false })))
  }, [])

  function selectContext(c) {
    setContext(c.name)
    setNamespace(null)
    setService(null)
    // Always show the known-accessible namespaces up front.
    setNamespaces(DEFAULT_NAMESPACES.map((name) => ({ name })))
    setServices([])
    setLoading((l) => ({ ...l, ns: true }))
    setErrors((x) => ({ ...x, ns: undefined }))
    api
      .listNamespaces(c.name)
      .then((discovered) => {
        // Merge discovered namespaces in, defaults first, no duplicates.
        const seen = new Set(DEFAULT_NAMESPACES)
        const extra = discovered.filter((n) => !seen.has(n.name))
        setNamespaces([...DEFAULT_NAMESPACES.map((name) => ({ name })), ...extra])
      })
      .catch((e) => setErrors((x) => ({ ...x, ns: e.message })))
      .finally(() => setLoading((l) => ({ ...l, ns: false })))
  }

  function selectNamespace(name) {
    setNamespace(name)
    setService(null)
    setServices([])
    setLoading((l) => ({ ...l, svc: true }))
    setErrors((x) => ({ ...x, svc: undefined }))
    api
      .listServices(context, name)
      .then(setServices)
      .catch((e) => setErrors((x) => ({ ...x, svc: e.message })))
      .finally(() => setLoading((l) => ({ ...l, svc: false })))
  }

  // Add a manually-typed namespace (needed when the token lacks cluster-wide
  // "list namespaces" permission) and select it.
  function addNamespace(name) {
    const trimmed = name.trim()
    if (!trimmed) return
    setNamespaces((prev) =>
      prev.some((n) => n.name === trimmed) ? prev : [...prev, { name: trimmed }],
    )
    selectNamespace(trimmed)
  }

  // A 403 on namespace listing means the token is namespace-scoped, not that
  // something is broken — surface a gentler hint and rely on manual entry.
  const nsForbidden = errors.ns && /forbidden|403/i.test(errors.ns)

  async function startForward(svc, port) {
    // In proxy mode the app reverse-proxies through the selected cluster's API
    // server to the service; just open the proxy URL in a new tab.
    if (proxyMode) {
      const prefix = config?.proxyPrefix || '/proxy/'
      const url =
        `${prefix}${encodeURIComponent(context)}/${encodeURIComponent(namespace)}` +
        `/${encodeURIComponent(svc.name)}/${port.port}/`
      window.open(url, '_blank', 'noopener')
      return
    }
    setStarting(true)
    setStartError(null)
    try {
      await api.startForward({
        context,
        namespace,
        service: svc.name,
        remotePort: port.port,
        localPort: 0, // let kubectl pick a free local port
      })
      onForwardStarted()
    } catch (e) {
      setStartError(e.message)
    } finally {
      setStarting(false)
    }
  }

  return (
    <div className="browse">
      <div className="columns">
        <Column
          title="Clusters"
          items={contexts}
          selected={context}
          onSelect={selectContext}
          loading={loading.ctx}
          error={errors.ctx}
          filterKey={(c) => c.name}
          render={(c) => (
            <>
              <span className="row-title">
                {c.name}
                {c.current && <span className="pill">current</span>}
              </span>
              <span className="row-sub">{c.cluster}</span>
            </>
          )}
        />

        <NamespaceColumn
          namespaces={namespaces}
          selected={namespace}
          onSelect={selectNamespace}
          onAdd={addNamespace}
          loading={loading.ns}
          error={errors.ns}
          forbidden={nsForbidden}
          hasContext={!!context}
        />

        <div className="column">
          <div className="column-head">
            <h2>Services</h2>
          </div>
          <div className="list">
            {loading.svc && <div className="hint">Loading…</div>}
            {errors.svc && <div className="error">{errors.svc}</div>}
            {!loading.svc && !errors.svc && !namespace && (
              <div className="hint">Select a namespace.</div>
            )}
            {!loading.svc && !errors.svc && namespace && services.length === 0 && (
              <div className="hint">No services.</div>
            )}
            {!loading.svc &&
              services.map((svc) => (
                <div
                  key={svc.name}
                  className={service === svc.name ? 'svc selected' : 'svc'}
                  onClick={() => setService(svc.name)}
                >
                  <div className="svc-head">
                    <span className="row-title">
                      {svc.displayName || svc.name}
                    </span>
                    <span className="tag">{svc.type}</span>
                  </div>
                  {svc.displayName && svc.displayName !== svc.name && (
                    <span className="row-sub svc-realname">{svc.name}</span>
                  )}
                  <div className="ports">
                    {(svc.ports?.length ?? 0) === 0 && (
                      <span className="row-sub">no ports</span>
                    )}
                    {(svc.ports ?? []).map((p) => (
                      <button
                        key={`${p.port}-${p.name}`}
                        className="port-btn"
                        disabled={starting}
                        title={
                          proxyMode
                            ? `Open ${svc.name}:${p.port} via in-cluster proxy (HTTP only)`
                            : `Forward service port ${p.port} → local`
                        }
                        onClick={(e) => {
                          e.stopPropagation()
                          startForward(svc, p)
                        }}
                      >
                        {p.name ? `${p.name} ` : ''}
                        {p.port}/{p.protocol || 'TCP'} {proxyMode ? '↗' : '→'}
                      </button>
                    ))}
                  </div>
                </div>
              ))}
          </div>
        </div>
      </div>

      {startError && <div className="banner error">{startError}</div>}
    </div>
  )
}
