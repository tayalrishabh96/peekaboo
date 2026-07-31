import { api } from './api'

const statusColors = {
  running: 'ok',
  starting: 'warn',
  error: 'err',
  stopped: 'muted',
}

export default function Forwards({ forwards, refresh }) {
  async function stop(id) {
    try {
      await api.stopForward(id)
    } finally {
      refresh()
    }
  }

  const sorted = [...forwards].sort((a, b) => Number(a.id) - Number(b.id))

  if (sorted.length === 0) {
    return (
      <div className="empty">
        <p>No active port-forwards.</p>
        <p className="hint">
          Go to the <strong>Browse</strong> tab, pick a cluster → namespace →
          service, and click a port to start forwarding.
        </p>
      </div>
    )
  }

  return (
    <div className="forwards">
      {sorted.map((f) => {
        const url = f.localPort ? `http://127.0.0.1:${f.localPort}` : null
        return (
          <div key={f.id} className="fwd-card">
            <div className="fwd-main">
              <div className="fwd-title">
                <span className={`dot ${statusColors[f.status] || 'muted'}`} />
                {f.service}
                <span className="row-sub">
                  {f.context} / {f.namespace}
                </span>
              </div>
              <div className="fwd-mapping">
                {f.localPort ? (
                  <a href={url} target="_blank" rel="noreferrer">
                    localhost:{f.localPort}
                  </a>
                ) : (
                  <span className="hint">assigning local port…</span>
                )}
                <span className="arrow">→</span>
                <span>
                  {f.service}:{f.remotePort}
                </span>
              </div>
              {f.error && <div className="error small">{f.error}</div>}
            </div>
            <div className="fwd-actions">
              <span className={`status ${statusColors[f.status] || 'muted'}`}>
                {f.status}
              </span>
              <button className="stop" onClick={() => stop(f.id)}>
                Stop
              </button>
            </div>
          </div>
        )
      })}
    </div>
  )
}
