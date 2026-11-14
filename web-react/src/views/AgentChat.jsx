import React, { useEffect, useMemo, useRef, useState } from 'react'
import { useApp } from '../store.jsx'
import { nav } from '../App.jsx'
import { Btn, Empty, I } from '../ui.jsx'
import { api, post, f2, relTime } from '../api.js'
import { parseIntent, VIEW_TITLE } from '../agent.js'

// 试聊对话记录：浏览器本地持久化。React 版与内嵌原生版共用同一个 key 与数据结构。
// { v:1, activeId, sessions:[{ id,title,model,msgs,createdAt,updatedAt }] }
// 时间戳为「秒」（可带小数以保序），可直接复用 api.js 的 relTime()。
const STORE_KEY = 'ss-agent-chats-v1'
const MAX_SESSIONS = 50
const MAX_MSGS = 200
// /api/models/try 的服务端限制：最多 12 条、单条 2000 字、合计 8000 字、末条必须是 user
const CTX_MAX_MSGS = 12
const CTX_MAX_CHARS = 8000
const CTX_MAX_ONE = 2000

const SUGGEST = [
  '生成一张赛博朋克城市夜景的图',
  '查看我的任务进度',
  '充值 100 点',
  '在市场搜索 Flux',
  '租一台云主机',
  '我的节点在线吗',
]

// 保留毫秒精度（秒的小数），同一秒内连续产生的对话也能按更新时间正确排序
const now = () => Date.now() / 1000
const uid = () => 'c' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7)
const newSession = model => ({ id: uid(), title: '新对话', model: model || '', msgs: [], createdAt: now(), updatedAt: now() })
const titleOf = text => {
  const s = String(text || '').replace(/\s+/g, ' ').trim()
  return s ? (s.length > 22 ? s.slice(0, 22) + '…' : s) : '新对话'
}
const summaryOf = list => {
  for (let i = list.length - 1; i >= 0; i--) {
    const m = list[i]
    if ((m.role === 'user' || m.role === 'assistant') && m.content) {
      return (m.role === 'user' ? '我：' : '') + String(m.content).replace(/\s+/g, ' ')
    }
  }
  return '还没有消息'
}
// 长对话按后端上限裁剪后再发送，避免历史记录累积后试聊直接报错
const buildContext = list => {
  const arr = list
    .filter(m => (m.role === 'user' || m.role === 'assistant') && String(m.content || '').trim())
    .map(m => ({ role: m.role, content: String(m.content).slice(0, CTX_MAX_ONE) }))
  const out = arr.slice(-CTX_MAX_MSGS)
  while (out.length > 1 && out.reduce((n, m) => n + m.content.length, 0) > CTX_MAX_CHARS) out.shift()
  while (out.length && out[out.length - 1].role !== 'user') out.pop()
  while (out.length && out[0].role !== 'user') out.shift()
  return out
}
const freshState = () => {
  const s = newSession('')
  return { v: 1, activeId: s.id, sessions: [s] }
}
function loadState() {
  try {
    const raw = JSON.parse(localStorage.getItem(STORE_KEY) || 'null')
    if (!raw || !Array.isArray(raw.sessions)) return freshState()
    const sessions = raw.sessions
      .filter(s => s && typeof s.id === 'string' && Array.isArray(s.msgs))
      .slice(-MAX_SESSIONS)
      .map(s => ({
        id: s.id, title: String(s.title || '新对话'), model: String(s.model || ''),
        msgs: s.msgs.slice(-MAX_MSGS),
        createdAt: Number(s.createdAt) || now(), updatedAt: Number(s.updatedAt) || now(),
      }))
    if (!sessions.length) return freshState()
    return { v: 1, activeId: sessions.some(s => s.id === raw.activeId) ? raw.activeId : sessions[0].id, sessions }
  } catch (e) { return freshState() }
}
// 返回真正落盘的状态：配额不足时丢弃最旧的会话再试，避免「历史记录静默丢失」
function persist(st) {
  const write = s => {
    try { localStorage.setItem(STORE_KEY, JSON.stringify(s)); return true } catch (e) { return false }
  }
  if (write(st)) return st
  let keep = st.sessions.slice().sort((a, b) => b.updatedAt - a.updatedAt)
  for (let n = 0; n < 3 && keep.length > 1; n++) {
    keep = keep.slice(0, Math.max(1, Math.floor(keep.length / 2)))
    const alt = { ...st, sessions: keep, activeId: keep.some(s => s.id === st.activeId) ? st.activeId : keep[0].id }
    if (write(alt)) return alt
  }
  return st
}

// 切换 tab 会卸载组件：内存快照保证禁用 localStorage 时当前对话也不会丢
let CACHE = null

export default function AgentChat() {
  const { me, jobs, workers, confirm, setPrefill } = useApp()
  const [state, setState] = useState(() => CACHE || loadState())
  const stateRef = useRef(state)
  const [input, setInput] = useState('')
  const [busyIds, setBusyIds] = useState([])
  const [catalog, setCatalog] = useState([])
  const [model, setModel] = useState(() => (state.sessions.find(s => s.id === state.activeId) || {}).model || '')
  const defaultModelRef = useRef('')
  const listRef = useRef(null)
  const taRef = useRef(null)
  const wrapRef = useRef(null)

  const { sessions, activeId } = state
  const active = sessions.find(s => s.id === activeId) || sessions[0] || { msgs: [], title: '新对话' }
  const msgs = active.msgs
  const convo = msgs.length > 0
  const busy = busyIds.includes(activeId)
  const ordered = useMemo(() => sessions.slice().sort((a, b) => b.updatedAt - a.updatedAt), [sessions])

  // 所有写入都基于同步 ref，异步回调（确认框、模型请求）始终落到发起时的那条对话
  const commit = fn => {
    const draft = fn(stateRef.current)
    if (!draft || draft === stateRef.current) return
    const next = persist(draft)
    stateRef.current = next
    CACHE = next
    setState(next)
  }
  const updateSession = (sid, fn) => commit(st => {
    const i = st.sessions.findIndex(s => s.id === sid)
    if (i < 0) return st
    const s = fn(st.sessions[i])
    if (!s) return st
    const next = st.sessions.slice()
    next[i] = s
    return { ...st, sessions: next }
  })
  const pushTo = (sid, m) => updateSession(sid, s => ({
    ...s,
    msgs: [...s.msgs, m].slice(-MAX_MSGS),
    title: (!s.msgs.length && m.role === 'user') ? titleOf(m.content) : s.title,
    updatedAt: now(),
  }))
  const markBusy = (sid, on) => setBusyIds(prev =>
    on ? (prev.includes(sid) ? prev : [...prev, sid]) : prev.filter(x => x !== sid))

  useEffect(() => {
    const fit = () => {
      const el = wrapRef.current
      if (!el) return
      if (!convo) { el.style.height = ''; return }
      const top = el.getBoundingClientRect().top
      el.style.height = Math.max(320, window.innerHeight - top - 16) + 'px'
    }
    fit()
    window.addEventListener('resize', fit)
    return () => window.removeEventListener('resize', fit)
  }, [convo, activeId])

  useEffect(() => {
    api('/api/models/catalog').then(d => {
      const c = d.models || []
      setCatalog(c)
      if (!c.length) return
      const def = (c.find(x => x.official) || c[0]).id
      defaultModelRef.current = def
      const cur = stateRef.current
      const s = cur.sessions.find(x => x.id === cur.activeId)
      const keep = s && c.some(x => x.id === s.model) ? s.model : def
      setModel(keep)
      if (s && s.model !== keep) updateSession(cur.activeId, x => ({ ...x, model: keep }))
    }).catch(() => {})
  }, [])
  useEffect(() => {
    if (listRef.current) listRef.current.scrollTop = listRef.current.scrollHeight
  }, [msgs, busy, activeId])

  const switchTo = sid => {
    if (sid === stateRef.current.activeId) return
    const s = stateRef.current.sessions.find(x => x.id === sid)
    // 历史会话里记录的模型可能已下架，回落到当前目录里的官方模型
    const saved = (s && s.model) || ''
    const m = !catalog.length ? saved
      : (catalog.some(c => c.id === saved) ? saved : defaultModelRef.current)
    commit(st => ({ ...st, activeId: sid }))
    setModel(m)
    if (s && s.model !== m) updateSession(sid, x => ({ ...x, model: m }))
    setInput('')
  }
  const newChat = () => {
    const st = stateRef.current
    const cur = st.sessions.find(s => s.id === st.activeId)
    // 已经是空对话时直接复用，不产生一堆无意义的空记录
    if (cur && !cur.msgs.length) { setInput(''); if (taRef.current) taRef.current.focus(); return }
    const s = newSession(model || defaultModelRef.current)
    commit(x => ({ ...x, sessions: [...x.sessions, s].slice(-MAX_SESSIONS), activeId: s.id }))
    setInput('')
    if (taRef.current) taRef.current.focus()
  }
  const clearChat = async () => {
    if (!msgs.length) return
    if (!await confirm('清空当前对话的全部消息？历史列表会保留这条对话。', '清空对话')) return
    updateSession(activeId, s => ({ ...s, msgs: [], title: '新对话', updatedAt: now() }))
  }
  const deleteChat = async (sid, e) => {
    e.stopPropagation()
    const target = stateRef.current.sessions.find(s => s.id === sid)
    if (!await confirm(`删除对话「${(target && target.title) || '新对话'}」？消息将无法恢复。`, '删除对话')) return
    const fallback = model || defaultModelRef.current
    commit(st => {
      const left = st.sessions.filter(s => s.id !== sid)
      if (!left.length) {
        const s = newSession(fallback)
        return { v: 1, activeId: s.id, sessions: [s] }
      }
      const activeId = st.activeId === sid
        ? left.slice().sort((a, b) => b.updatedAt - a.updatedAt)[0].id
        : st.activeId
      return { ...st, sessions: left, activeId }
    })
    const s = stateRef.current.sessions.find(x => x.id === stateRef.current.activeId)
    setModel((s && s.model) || fallback)
  }

  const modelChat = async (sid, hist, mdl) => {
    if (!mdl) { pushTo(sid, { role: 'error', content: '当前没有可用模型，请先在「上游接入」登记一个 OpenAI 兼容上游。' }); return }
    markBusy(sid, true)
    try {
      const d = await post('/api/models/try', { model: mdl, messages: hist })
      const content = d.choices && d.choices[0] && d.choices[0].message && d.choices[0].message.content
      const usage = d.usage ? `输入 ${d.usage.prompt_tokens || 0} · 输出 ${d.usage.completion_tokens || 0} tokens` : ''
      pushTo(sid, { role: 'assistant', content: content || '（模型没有返回文本内容）', usage, model: mdl })
    } catch (err) {
      const e = err && err.error
      pushTo(sid, { role: 'error', content: typeof e === 'string' ? e : (e && e.message) || String(err) })
    } finally { markBusy(sid, false) }
  }

  const run = async (sid, text) => {
    const cur = stateRef.current.sessions.find(s => s.id === sid)
    const hist = buildContext([...(cur ? cur.msgs : []), { role: 'user', content: text }])
    const mdl = model
    pushTo(sid, { role: 'user', content: text })
    const push = m => pushTo(sid, m)
    const pushAgent = (title, detail, extra) => push({ role: 'agent', title, detail, ...extra })
    const it = parseIntent(text)
    switch (it.type) {
      case 'nav':
        pushAgent('导航', `已打开「${VIEW_TITLE[it.view]}」`, { view: it.view })
        nav(it.view, it.tab); return
      case 'image':
        setPrefill({ view: 'image', tpl: { prompt: it.prompt } })
        pushAgent('生图', '已预填画面描述并打开生图工作台', { detail2: it.prompt, view: 'image' })
        nav('image'); return
      case 'video':
        setPrefill({ view: 'video', tpl: { prompt: it.prompt } })
        pushAgent('生视频', '已预填镜头描述并打开生视频工作台', { detail2: it.prompt, view: 'video' })
        nav('video'); return
      case 'render':
        setPrefill({ view: 'render', tpl: {}, kind: it.kind })
        pushAgent('渲染', `已打开渲染工作台（${it.kind === 'blender' ? 'Blender' : '视频转码'}），上传素材即可提交`, { view: 'render' })
        nav('render'); return
      case 'build':
        pushAgent('编排', '已打开任务编排，上传源码 zip 并填写命令即可提交', { view: 'build' })
        nav('build'); return
      case 'marketSearch':
        try { localStorage.setItem('ss-react-market', JSON.stringify({ q: it.q, cat: 'all', sort: 'new' })) } catch (e) {}
        pushAgent('搜索', `已在服务市场搜索「${it.q || '全部'}」`, { view: 'market' })
        nav('market'); return
      case 'vmRent':
        pushAgent('云主机', '已打开云主机租用列表，选择配置即可下单', { view: 'market' })
        nav('market', 'vm'); return
      case 'sellPublish':
        setPrefill({ view: 'sell', tpl: { title: it.title } })
        pushAgent('发布', '已打开我的发布并预填标题，补充配置后即可上架', { view: 'sell' })
        nav('sell'); return
      case 'forumPost':
        setPrefill({ view: 'forum', tpl: { title: it.title } })
        pushAgent('发帖', '已打开论坛并预填标题，补充正文后即可发布', { view: 'forum' })
        nav('forum'); return
      case 'createKey': {
        if (!await confirm(`创建 API Key「${it.name || '默认密钥'}」？密钥只显示一次。`, '创建 API Key')) { pushAgent('已取消', '未创建密钥'); return }
        try {
          const d = await post('/api/keys', { name: it.name || '默认密钥', daily_quota: 0 })
          pushAgent('密钥已创建', '请立即复制保存（仅显示一次）', { secret: d.key })
        } catch (e) { push({ role: 'error', content: '创建失败：' + e }) }
        return
      }
      case 'recharge': {
        if (!await confirm(`充值 ${f2(it.amount)} 算力点？`, '充值')) { pushAgent('已取消', '未充值'); return }
        try { await post('/api/me/recharge', { amount: it.amount }); pushAgent('充值成功', `已充值 ${f2(it.amount)} 算力点`) }
        catch (e) { push({ role: 'error', content: '充值失败：' + e }) }
        return
      }
      case 'balance':
        pushAgent('余额', `当前账户余额 ${f2(me ? me.credits : 0)} 算力点`, { view: 'wallet' }); return
      case 'tasks': {
        const run_ = jobs.filter(j => ['queued', 'running', 'pending'].includes(j.status)).length
        const done = jobs.filter(j => j.status === 'done').length
        const failed = jobs.filter(j => j.status === 'failed').length
        pushAgent('任务概览', `共 ${jobs.length} 个任务：进行中 ${run_} · 完成 ${done} · 失败 ${failed}`, { view: 'jobs' }); return
      }
      case 'nodes': {
        const mine = workers.filter(w => w.mine)
        const on = mine.filter(w => w.online).length
        const earn = mine.reduce((a, w) => a + Number(w.earned || 0), 0)
        pushAgent('节点概览', `我的节点 ${mine.length} 台，在线 ${on} 台，累计贡献 ${f2(earn)} 点`, { view: 'nodes' }); return
      }
      default:
        await modelChat(sid, hist, mdl)
    }
  }
  const submit = e => {
    e.preventDefault()
    const text = input.trim()
    if (!text || busy) return
    setInput('')
    run(activeId, text)
  }
  const composer = (
    <form className="agent-composer" onSubmit={submit}>
      <textarea ref={taRef} rows={convo ? 2 : 3} value={input} maxLength={CTX_MAX_ONE}
        onChange={e => setInput(e.target.value)}
        placeholder="一句话下达指令，例如：生成一张赛博城市图 / 查看我的任务 / 充值 100 点；也可以直接提问模型"
        onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(e) } }} />
      <div className="agent-composer-row">
        <select value={model} onChange={e => {
          const v = e.target.value
          setModel(v)
          updateSession(activeId, s => ({ ...s, model: v }))
        }} aria-label="回落模型" title="无法识别为指令时回落该模型对话">
          {catalog.length ? catalog.map(m => <option key={m.id} value={m.id}>{m.id}</option>) : <option value="">（无可用模型）</option>}
        </select>
        <span className="hint" style={{ margin: 0 }}>Enter 发送 · Shift+Enter 换行</span>
        <Btn ghost tiny type="button" onClick={clearChat} disabled={!msgs.length}>清空</Btn>
        <Btn type="submit" disabled={busy || !input.trim()} style={{ marginLeft: 'auto' }}>{busy ? '执行中…' : '发送'}</Btn>
      </div>
    </form>
  )

  return (
    <div className="try-layout">
      <aside className="card try-side">
        <div className="try-side-head"><b>历史对话</b><span className="try-count">{sessions.length}</span></div>
        <Btn block type="button" onClick={newChat}><I name="plus" size={14} /> 新建对话</Btn>
        <div className="try-conv-list">
          {ordered.map(s => (
            <div key={s.id} className={'try-conv' + (s.id === activeId ? ' on' : '')}>
              <button type="button" className="try-conv-main" onClick={() => switchTo(s.id)} title={s.title}>
                <span className="try-conv-top"><b>{s.title}</b><span className="try-conv-time">{relTime(s.updatedAt)}</span></span>
                <span className="try-conv-last">{summaryOf(s.msgs)}</span>
              </button>
              <button type="button" className="try-conv-del" onClick={e => deleteChat(s.id, e)}
                aria-label={`删除对话 ${s.title}`} title="删除对话"><I name="x" size={13} /></button>
            </div>
          ))}
          {ordered.length === 1 && !ordered[0].msgs.length &&
            <Empty>还没有历史对话，发第一条消息后会自动保存。</Empty>}
        </div>
        <div className="try-side-foot">对话记录仅保存在本浏览器；长对话自动裁剪为最近 {CTX_MAX_MSGS} 条上下文。</div>
      </aside>

      <section className="try-main">
        <div className="try-chat-head">
          <div className="try-chat-meta">
            <I name="messages" size={14} />
            <b title={active.title}>{active.title}</b>
            <span>{msgs.length} 条消息</span>
            {active.model && <span className="try-chat-model" title={active.model}>{active.model}</span>}
          </div>
          <Btn ghost tiny type="button" onClick={newChat}><I name="plus" size={13} /> 新对话</Btn>
        </div>
        <div className={'agent-wrap' + (convo ? ' convo' : '')} ref={wrapRef}>
          {convo ? (
            <div className="agent-msgs" ref={listRef}>
              {msgs.map((m, i) => {
                if (m.role === 'user') return <div key={i} className="bubble me"><span className="who-line">我</span>{m.content}</div>
                if (m.role === 'assistant') return <div key={i} className="bubble"><span className="who-line">{m.model || model || '模型'}</span>{m.content}{m.usage && <span className="usage">{m.usage}</span>}</div>
                if (m.role === 'error') return <div key={i} className="bubble error"><span className="who-line">提示</span>{m.content}</div>
                return (
                  <div key={i} className="agent-card">
                    <span className="agent-card-ic"><I name="build" size={15} /></span>
                    <div className="agent-card-main">
                      <b>{m.title}</b>
                      <span>{m.detail}</span>
                      {m.detail2 && <code>{m.detail2}</code>}
                      {m.secret && <pre>{m.secret}</pre>}
                    </div>
                    {m.view && <Btn ghost tiny onClick={() => nav(m.view)}>打开 →</Btn>}
                  </div>
                )
              })}
              {busy && <div className="bubble"><span className="who-line">agent</span>正在执行…</div>}
            </div>
          ) : (
            <div className="agent-hero">
              <span className="eyebrow">Starstack Agent</span>
              <h2>一句话，直达你想做的事</h2>
              <p>输入自然语言指令：导航到任意界面、预填创作参数、搜索市场、充值、查余额/任务/节点；识别不了时自动回落模型对话。</p>
              <div className="chips sm agent-suggest">
                {SUGGEST.map(s => <button key={s} type="button" className="chip" onClick={() => { setInput(s); taRef.current && taRef.current.focus() }}>{s}</button>)}
              </div>
            </div>
          )}
          {composer}
        </div>
      </section>
    </div>
  )
}
