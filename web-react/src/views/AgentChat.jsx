import React, { useEffect, useRef, useState } from 'react'
import { useApp } from '../store.jsx'
import { nav } from '../App.jsx'
import { Btn, Tag, Empty, I } from '../ui.jsx'
import { api, post, f2 } from '../api.js'
import { parseIntent, VIEW_TITLE } from '../agent.js'

// 会话历史放在模块级，跨视图切换保留（agent 导航离开后再回来仍在）
let HISTORY = []
const SUGGEST = [
  '生成一张赛博朋克城市夜景的图',
  '查看我的任务进度',
  '充值 100 点',
  '在市场搜索 Flux',
  '租一台云主机',
  '我的节点在线吗',
]
export default function AgentChat() {
  const app = useApp()
  const { me, jobs, workers, toast, confirm, setPrefill } = app
  const [msgs, setMsgs] = useState(HISTORY)
  const convo = msgs.length > 0
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [catalog, setCatalog] = useState([])
  const [model, setModel] = useState('')
  const listRef = useRef(null)
  const taRef = useRef(null)
  const wrapRef = useRef(null)

  //  conversation 模式：撑满剩余视口，消息内部滚动、输入框固定区底
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
  }, [convo])

  useEffect(() => { HISTORY = msgs }, [msgs])
  useEffect(() => {
    api('/api/models/catalog').then(d => {
      const c = d.models || []; setCatalog(c)
      if (c.length) setModel(m => m || (c.find(x => x.official) || c[0]).id)
    }).catch(() => {})
  }, [])
  useEffect(() => {
    if (listRef.current) listRef.current.scrollTop = listRef.current.scrollHeight
  }, [msgs, busy])

  const push = m => setMsgs(prev => [...prev, m])
  const pushAgent = (title, detail, extra) => push({ role: 'agent', title, detail, ...extra })

  const modelChat = async text => {
    if (!model) { push({ role: 'error', content: '当前没有可用模型，请先在「上游接入」登记一个 OpenAI 兼容上游。' }); return }
    const hist = [...msgs, { role: 'user', content: text }].filter(m => m.role === 'user' || m.role === 'assistant')
    setBusy(true)
    try {
      const d = await post('/api/models/try', { model, messages: hist.map(m => ({ role: m.role, content: m.content })) })
      const content = d.choices && d.choices[0] && d.choices[0].message && d.choices[0].message.content
      const usage = d.usage ? `输入 ${d.usage.prompt_tokens || 0} · 输出 ${d.usage.completion_tokens || 0} tokens` : ''
      push({ role: 'assistant', content: content || '（模型没有返回文本内容）', usage, model })
    } catch (err) {
      const e = err && err.error
      push({ role: 'error', content: typeof e === 'string' ? e : (e && e.message) || String(err) })
    } finally { setBusy(false) }
  }

  const run = async text => {
    push({ role: 'user', content: text })
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
        await modelChat(text)
    }
  }
  const submit = e => {
    e.preventDefault()
    const text = input.trim()
    if (!text || busy) return
    setInput('')
    run(text)
  }
  const composer = (
    <form className="agent-composer" onSubmit={submit}>
      <textarea ref={taRef} rows={convo ? 2 : 3} value={input} onChange={e => setInput(e.target.value)}
        placeholder="一句话下达指令，例如：生成一张赛博城市图 / 查看我的任务 / 充值 100 点；也可以直接提问模型"
        onKeyDown={e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(e) } }} />
      <div className="agent-composer-row">
        <select value={model} onChange={e => setModel(e.target.value)} aria-label="回落模型" title="无法识别为指令时回落该模型对话">
          {catalog.length ? catalog.map(m => <option key={m.id} value={m.id}>{m.id}</option>) : <option value="">（无可用模型）</option>}
        </select>
        <span className="hint" style={{ margin: 0 }}>Enter 发送 · Shift+Enter 换行</span>
        <Btn ghost tiny type="button" onClick={() => { setMsgs([]); HISTORY = [] }}>清空</Btn>
        <Btn type="submit" disabled={busy || !input.trim()} style={{ marginLeft: 'auto' }}>{busy ? '执行中…' : '发送'}</Btn>
      </div>
    </form>
  )

  return (
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
  )
}