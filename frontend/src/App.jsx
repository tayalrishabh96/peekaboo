import { useEffect, useState, useCallback } from 'react'
import { api } from './api'
import Browse from './Browse.jsx'
import Forwards from './Forwards.jsx'

export default function App() {
  const [tab, setTab] = useState('browse')
  const [forwards, setForwards] = useState([])
  // "portforward" (local) or "proxy" (in-cluster). Defaults to portforward
  // until the backend config loads.
  const [config, setConfig] = useState({ forwardMode: 'portforward', proxyPrefix: '/proxy/' })

  const refreshForwards = useCallback(async () => {
    try {
      setForwards(await api.listForwards())
    } catch {
      /* backend may be momentarily unavailable */
    }
  }, [])

  useEffect(() => {
    api.getConfig().then(setConfig).catch(() => {})
  }, [])

  useEffect(() => {
    // Only poll forwards in port-forward mode; proxy mode has no forwards.
    if (config.forwardMode !== 'portforward') return
    refreshForwards()
    const t = setInterval(refreshForwards, 2000)
    return () => clearInterval(t)
  }, [refreshForwards, config.forwardMode])

  // Called by Browse when a forward is started: switch to the Forwards tab.
  const onForwardStarted = useCallback(async () => {
    await refreshForwards()
    setTab('forwards')
  }, [refreshForwards])

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="logo">⇄</span> kube-forwarder
        </div>
        <nav className="tabs">
          <button
            className={tab === 'browse' ? 'tab active' : 'tab'}
            onClick={() => setTab('browse')}
          >
            Browse
          </button>
          {config.forwardMode === 'portforward' && (
            <button
              className={tab === 'forwards' ? 'tab active' : 'tab'}
              onClick={() => setTab('forwards')}
            >
              Port Forwards
              {forwards.length > 0 && <span className="badge">{forwards.length}</span>}
            </button>
          )}
        </nav>
        {config.forwardMode === 'proxy' && (
          <span className="mode-note">proxy mode · in-cluster</span>
        )}
      </header>

      <main className="content">
        {tab === 'browse' || config.forwardMode !== 'portforward' ? (
          <Browse onForwardStarted={onForwardStarted} config={config} />
        ) : (
          <Forwards forwards={forwards} refresh={refreshForwards} />
        )}
      </main>
    </div>
  )
}
