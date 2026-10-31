import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, store } from './api'
import { AppCtx } from './ctx'
import { f2 } from '../../shared/index.js'
import Auth from './views/Auth'
import Submit from './views/Submit'
import Jobs from './views/Jobs'
import Market from './views/Market'
import Vm from './views/Vm'
import Nodes from './views/Nodes'
import ApiView from './views/ApiView'
import Bill from './views/Bill'
import Forum from './views/Forum'
import Me from './views/Me'

const NAV = [
  ['submit', '提交'],
  ['jobs', '任务'],
  ['market', '市场'],
  ['vm', '云主机'],
  ['nodes', '节点'],
  ['api', '模型 API'],
  ['bill', '账单'],
  ['forum', '论坛'],
  ['me', '我的'],
]

const VIEWS: Record<string, any> = { submit: Submit, jobs: Jobs, market: Market, vm: Vm, nodes: Nodes, api: ApiView, bill: Bill, forum: Forum, me: Me }

export default function App() {
  const [theme, setTheme] = useState(store.get('ss-theme') || 'light')
  const [me, setMe] = useState<any>(null)
  const [policy, setPolicy] = useState<any>(null)
  const [workers, setWorkers] = useState<any[]>([])
  const [jobs, setJobs] = useState<any[]>([])
  const [view, setView] = useState('submit')
  const [booting, setBooting] = useState(true)
  const [toast, setToast] = useState<{ msg: string; tone?: string } | null>(null)
  const toastTimer = useRef<any>(null)

  const showToast = useCallback((msg: string, tone?: string) => {
    setToast({ msg, tone })
    clearTimeout(toastTimer.current)
    toastTimer.current = setTimeout(() => setToast(null), 2600)
  }, [])

  useEffect(() => {
    document.documentElement.dataset.theme = theme
    store.set('ss-theme', theme)
  }, [theme])

  const signOut = useCallback(() => {
    api.logout().catch(() => {})
    store.set('ss-token', '')
    setMe(null)
    setJobs([])
    setWorkers([])
    setView('submit')
  }, [])

  // Bootstrap: read the deployment policy before login so the auth card can
  // advertise the signup grant, then resume a stored token.
  useEffect(() => {
    ;(async () => {
      try {
        const st = await api.state()
        if (st.policy) setPolicy(st.policy)
      } catch {
        /* broker offline: stay on the auth screen with defaults */
      }
      try {
        const m = await api.me()
        if (m.signed_in) setMe(m)
      } catch {
        /* no session */
      }
      setBooting(false)
    })()
  }, [])

  const signedIn = !!me

  useEffect(() => {
    if (!signedIn) return
    let alive = true
    let timer: any
    const tick = async () => {
      try {
        const st = await api.state()
        if (!alive) return
        if (!st.signed_in) {
          signOut()
          return
        }
        setMe(st.me)
        setWorkers((st.workers || []).slice().sort((a: any, b: any) => b.online - a.online || b.mine - a.mine || String(a.name || a.id).localeCompare(String(b.name || b.id), 'zh-CN')))
        setJobs(st.jobs || [])
        if (st.policy) setPolicy(st.policy)
      } catch (e: any) {
        if (e && e.status === 401) {
          signOut()
          return
        }
      }
      timer = setTimeout(tick, 2000)
    }
    tick()
    return () => {
      alive = false
      clearTimeout(timer)
    }
  }, [signedIn, signOut])

  const ctx = useMemo(
    () => ({ api, me, policy, workers, jobs, view, setView, showToast, signOut, setMe }),
    [me, policy, workers, jobs, view, showToast, signOut],
  )

  if (booting) {
    return (
      <div className="app">
        <div className="main" style={{ display: 'grid', placeItems: 'center', minHeight: '70vh' }}>
          <div className="empty">
            <span className="spin" /> 正在连接算力池…
          </div>
        </div>
      </div>
    )
  }

  if (!me) {
    return (
      <AppCtx.Provider value={ctx}>
        <div className="app">
          <div className="main">
            <Auth
              policy={policy}
              onDone={(m: any) => {
                setMe(m)
                setView('submit')
              }}
            />
          </div>
          {toast && <div className={'toast' + (toast.tone ? ' ' + toast.tone : '')}>{toast.msg}</div>}
        </div>
      </AppCtx.Provider>
    )
  }

  const View = VIEWS[view] || Submit

  return (
    <AppCtx.Provider value={ctx}>
      <div className="app">
        <header className="header">
          <div className="header-row">
            <div className="brand">
              <div className="mark">星</div>
              <div>
                <b>星栈算力</b>
                <span>Compute Share</span>
              </div>
            </div>
            <div className="wallet">
              余额 <b>{f2(me.credits)}</b>
            </div>
            <button className="iconbtn" aria-label="切换深浅色" onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>
              <svg className="i-sun" viewBox="0 0 24 24">
                <circle cx="12" cy="12" r="4" />
                <path d="M12 2v2m0 16v2M2 12h2m16 0h2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M19.1 4.9l-1.4 1.4M6.3 17.7l-1.4 1.4" />
              </svg>
              <svg className="i-moon" viewBox="0 0 24 24">
                <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z" />
              </svg>
            </button>
          </div>
          <nav className="tabs">
            {NAV.map(([v, label]) => (
              <button key={v} className={view === v ? 'on' : ''} onClick={() => setView(v)}>
                {label}
              </button>
            ))}
          </nav>
        </header>
        <main className="main">
          <View />
        </main>
        {toast && <div className={'toast' + (toast.tone ? ' ' + toast.tone : '')}>{toast.msg}</div>}
      </div>
    </AppCtx.Provider>
  )
}
