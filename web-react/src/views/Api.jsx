import React, { useCallback, useEffect, useState } from 'react'
import { useApp } from '../store.jsx'
import { Card, Tabs, Btn, Tag, Empty, Field } from '../ui.jsx'
import { api, post, f2, apiTS, ctxLabel, capNames, href, BASE } from '../api.js'
import AgentChat from './AgentChat.jsx'

const CAPS = ['tools', 'json_object', 'json_schema', 'regex', 'vision', 'reasoning', 'deterministic', 'prefix_cache']
export default function Api({ tab }) {
  const { toast, confirm } = useApp()
  const [t, setT] = useState(tab || 'try')
  useEffect(() => { if (tab) setT(tab) }, [tab])
  const [keys, setKeys] = useState([])
  const [ups, setUps] = useState([])
  const [cat, setCat] = useState([])
  const [usage, setUsage] = useState(null)
  const [exModel, setExModel] = useState('')
  const [keyName, setKeyName] = useState('')
  const [keyDaily, setKeyDaily] = useState('0')
  const [newKey, setNewKey] = useState('')
  const [up, setUp] = useState({ name: '', base_url: '', api_key: '', price: '0.5', price_out: '0', price_cached: '0', max_context: '32768', models: '', caps: [] })
  const [upMsg, setUpMsg] = useState('')

  const load = useCallback(async () => {
    try {
      const [k, u, c, g] = await Promise.all([api('/api/keys'), api('/api/models/upstreams'), api('/api/models/catalog'), api('/api/models/usage?days=7').catch(() => null)])
      setKeys(k.keys || []); setUps(u.upstreams || []); setCat(c.models || []); setUsage(g)
      if (c.models && c.models.length) {
        const m = c.models.find(x => x.official) || c.models[0]
        setModel(prev => prev || m.id); setExModel(prev => prev || m.id)
      }
    } catch (e) {}
  }, [])
  useEffect(() => { load() }, [t, load])
  const createKey = async () => {
    try {
      const d = await post('/api/keys', { name: keyName.trim() || '默认密钥', daily_quota: +keyDaily || 0 })
      setNewKey(d.key); setKeyName(''); setKeyDaily('0'); load()
      toast('密钥已创建，请立即复制保存', 'ok')
    } catch (e) { toast(String(e), 'bad', '创建失败') }
  }
  const revoke = async id => {
    if (!await confirm('撤销后使用该密钥的调用会立即失效，确定？', '撤销 API Key')) return
    try { await api('/api/keys/' + encodeURIComponent(id), { method: 'DELETE' }); load(); toast('密钥已撤销', 'ok') } catch (e) { toast(String(e), 'bad') }
  }
  const probe = async () => {
    setUpMsg('正在请求上游 /v1/models…')
    try {
      const d = await post('/api/models/upstreams/probe', { base_url: up.base_url.trim(), api_key: up.api_key })
      setUpMsg(`探测成功（${d.latency_ms} ms）：${(d.models || []).join('、')}`)
      if (!up.models.trim()) setUp(u => ({ ...u, models: (d.models || []).slice(0, 8).join(', ') }))
    } catch (e) { setUpMsg('探测失败：' + e) }
  }
  const addUp = async () => {
    const body = { name: up.name.trim(), base_url: up.base_url.trim(), api_key: up.api_key, models: up.models.trim(), price: +up.price, price_out: +up.price_out || 0, price_cached: +up.price_cached || 0, max_context: +up.max_context || 32768, caps: up.caps.join(',') }
    if (!body.base_url) { setUpMsg('请填写 Base URL'); return }
    if (!body.models) { setUpMsg('请填写至少一个模型名'); return }
    if (!(body.price > 0)) { setUpMsg('输入单价必须大于 0'); return }
    try {
      await post('/api/models/upstreams', body)
      setUpMsg('已登记，模型已进入可用目录')
      setUp({ name: '', base_url: '', api_key: '', price: '0.5', price_out: '0', price_cached: '0', max_context: '32768', models: '', caps: [] })
      load(); toast('上游已登记', 'ok')
    } catch (e) { setUpMsg('登记失败：' + e) }
  }
  const toggleUp = async (id, enabled) => {
    try { await api('/api/models/upstreams/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) }); load() } catch (e) { toast(String(e), 'bad') }
  }
  const delUp = async id => {
    if (!await confirm('删除后这个上游立即退出路由，确定？', '删除上游')) return
    try { await api('/api/models/upstreams/' + encodeURIComponent(id), { method: 'DELETE' }); load(); toast('上游已删除', 'ok') } catch (e) { toast(String(e), 'bad') }
  }
  const curl = `curl ${location.origin + BASE}/v1/chat/completions \\\n  -H "Authorization: Bearer ${newKey || 'sk-ss-...'}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${exModel || 'your-model'}","messages":[{"role":"user","content":"你好"}]}'`

  return (
    <div>
      <div className="phead"><div className="phead-txt"><span className="eyebrow">Model Gateway</span><h1>模型 API</h1><p>官方 OpenAI 兼容网关：免密钥试聊、管理 API Key、登记模型上游，按实际 token 结算。</p></div></div>
      <Tabs items={[{ key: 'try', label: '试聊' }, { key: 'keys', label: '密钥与模型' }, { key: 'upstream', label: '上游接入' }, { key: 'usage', label: '用量对账' }]} active={t} onChange={setT} />
      {t === 'try' && <AgentChat />}
      {t === 'keys' && (
        <div className="grid">
          <Card kicker="Keys" title="我的 API Key">
            <div className="hint" style={{ marginTop: 0 }}>用 <code>Authorization: Bearer sk-ss-…</code> 调用 OpenAI 兼容接口。密钥只在创建时显示一次。</div>
            <div className="row" style={{ marginTop: 16 }}>
              <Field label="密钥名称"><input value={keyName} onChange={e => setKeyName(e.target.value)} placeholder="例如 论文实验" /></Field>
              <Field label="每日限额（点，0=不限）"><input type="number" min="0" step="0.1" value={keyDaily} onChange={e => setKeyDaily(e.target.value)} /></Field>
            </div>
            <div className="formfoot" style={{ marginTop: 14 }}><Btn onClick={createKey}>创建密钥</Btn></div>
            {newKey && (<div><div className="hint warn">新密钥只显示这一次，请立即复制保存：</div><pre>{newKey}</pre>
              <Btn ghost tiny onClick={async () => { try { await navigator.clipboard.writeText(newKey); toast('已复制', 'ok') } catch (e) { toast('复制失败，请手动选择', 'warn') } }}>复制密钥</Btn></div>)}
            <div className="tablewrap" style={{ marginTop: 18 }}>
              <table><thead><tr><th>名称</th><th>前缀</th><th>今日已用</th><th>限额</th><th>创建</th><th>最近使用</th><th /></tr></thead>
                <tbody>{keys.length ? keys.map(k => (
                  <tr key={k.id}><td>{k.name}</td><td className="num">{k.prefix}</td><td className="num">{f2(k.spent_today)}</td><td className="num">{k.daily_quota > 0 ? f2(k.daily_quota) + ' 点' : '不限'}</td><td className="num">{apiTS(k.created_at)}</td><td className="num">{k.last_used ? apiTS(k.last_used) : '—'}</td>
                    <td>{k.revoked ? <Tag tone="off">已撤销</Tag> : <Btn ghost tiny onClick={() => revoke(k.id)}>撤销</Btn>}</td></tr>
                )) : <tr><td colSpan={7} className="empty">还没有 API Key</td></tr>}</tbody></table>
            </div>
          </Card>
          <div>
            <Card kicker="Catalog" title="可用模型">
              <div className="tablewrap"><table><thead><tr><th>模型</th><th>上游数</th><th>最低输入单价</th><th>上下文</th><th>能力</th></tr></thead>
                <tbody>{cat.length ? cat.map(m => (
                  <tr key={m.id}><td className="num">{m.id} {m.official && <Tag tone="mine">官方</Tag>}</td><td>{m.providers}</td><td className="num">{f2(m.price_per_1k)} 点/千 token</td><td className="num">{ctxLabel(m.max_context)}</td>
                    <td>{(m.capabilities || []).filter(c => c !== 'chat').map(c => <Tag key={c}>{capNames[c] || c}</Tag>)}</td></tr>
                )) : <tr><td colSpan={5} className="empty">还没有可用模型</td></tr>}</tbody></table></div>
            </Card>
            <Card kicker="Example" title="调用示例">
              <label style={{ marginTop: 0 }}>模型</label>
              <select value={exModel} onChange={e => setExModel(e.target.value)}>{cat.map(m => <option key={m.id} value={m.id}>{m.id}</option>)}</select>
              <pre>{curl}</pre>
              <div className="hint">任意 OpenAI SDK 把 <code>base_url</code> 指到 <code>{location.origin + BASE}/v1</code> 即可。</div>
            </Card>
          </div>
        </div>
      )}
      {t === 'upstream' && (
        <div className="grid">
          <Card kicker="Upstream" title="接入模型上游">
            <Field label="上游名称"><input value={up.name} onChange={e => setUp({ ...up, name: e.target.value })} placeholder="例如 宿舍 4090 · Qwen3-30B-A3B" /></Field>
            <Field label="Base URL"><input value={up.base_url} onChange={e => setUp({ ...up, base_url: e.target.value })} placeholder="http://192.168.1.20:8080" /></Field>
            <div className="row">
              <Field label="上游 API Key（可选）"><input type="password" value={up.api_key} onChange={e => setUp({ ...up, api_key: e.target.value })} /></Field>
              <Field label="输入单价（点/千 token）"><input type="number" min="0.0001" step="0.1" value={up.price} onChange={e => setUp({ ...up, price: e.target.value })} /></Field>
            </div>
            <div className="row">
              <Field label="输出单价（0=同输入）"><input type="number" min="0" step="0.1" value={up.price_out} onChange={e => setUp({ ...up, price_out: e.target.value })} /></Field>
              <Field label="缓存命中单价（0=同输入）"><input type="number" min="0" step="0.1" value={up.price_cached} onChange={e => setUp({ ...up, price_cached: e.target.value })} /></Field>
            </div>
            <div className="row">
              <Field label="最大上下文（tokens）"><input type="number" min="1024" step="1024" value={up.max_context} onChange={e => setUp({ ...up, max_context: e.target.value })} /></Field>
              <Field label="声明能力">
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px 14px', marginTop: 9, fontSize: 12.5 }}>
                  {CAPS.map(c => <label key={c} style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 5, fontWeight: 400 }}>
                    <input type="checkbox" checked={up.caps.includes(c)} onChange={e => setUp({ ...up, caps: e.target.checked ? [...up.caps, c] : up.caps.filter(x => x !== c) })} />{capNames[c]}</label>)}
                </div>
              </Field>
            </div>
            <Field label="提供的模型名（逗号分隔）">
              <div style={{ display: 'flex', gap: 8 }}><input value={up.models} onChange={e => setUp({ ...up, models: e.target.value })} placeholder="qwen3-30b-a3b, qwen2.5-7b" /><Btn ghost onClick={probe} style={{ height: 38, flex: '0 0 auto' }}>探测</Btn></div>
            </Field>
            <div className="hint">{upMsg}</div>
            <div className="formfoot" style={{ marginTop: 14 }}><Btn onClick={addUp}>登记上游</Btn></div>
          </Card>
          <Card kicker="Registered" title="我登记的上游">
            {ups.length ? ups.map(u => {
              const h = u.health || {}
              return (
                <div className="card" key={u.id} style={{ marginBottom: 10, padding: '16px 18px' }}>
                  <h2 style={{ marginBottom: 10 }}>{u.name} {u.official && <Tag tone="mine">官方</Tag>} {u.enabled ? (h.cooling_sec ? <Tag tone="off">冷却 {h.cooling_sec}s</Tag> : <Tag tone="on">路由中</Tag>) : <Tag tone="off">已停用</Tag>}</h2>
                  <div className="hint" style={{ marginTop: 0 }}>{u.base_url} · {(u.models || []).join(' / ')}</div>
                  <div className="stats">
                    <div className="stat"><b>{f2(u.price)}</b><span>输入/千token</span></div>
                    <div className="stat"><b>{f2(u.price_out || u.price)}</b><span>输出/千token</span></div>
                    <div className="stat"><b>{ctxLabel(u.max_context)}</b><span>上下文</span></div>
                  </div>
                  {h.last_error && <div className="hint warn">最近错误：{h.last_error}</div>}
                  <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
                    <Btn ghost tiny onClick={() => toggleUp(u.id, !u.enabled)}>{u.enabled ? '停用' : '启用'}</Btn>
                    <Btn ghost tiny danger onClick={() => delUp(u.id)}>删除</Btn>
                  </div>
                </div>
              )
            }) : <Empty>还没有登记上游。先填一个可访问的 OpenAI 兼容地址。</Empty>}
          </Card>
        </div>
      )}
      {t === 'usage' && (
        <Card kicker="Reconcile" title="上游用量对账（近 7 天）">
          {!usage ? <div className="hint" style={{ margin: 0 }}>用量对账仅平台自营账户可查看。</div> : (
            <>
              <div className="tablewrap"><table><thead><tr><th>上游</th><th>调用</th><th>失败</th><th>成本（点）</th><th>输入</th><th>输出</th></tr></thead>
                <tbody>{(usage.by_upstream || []).map((x, i) => <tr key={i}><td>{x.name || x.id}</td><td className="num">{x.requests}</td><td className="num">{x.failed}</td><td className="num">{f2(x.cost)}</td><td className="num">{x.prompt}</td><td className="num">{x.completion}</td></tr>)}</tbody></table></div>
              <div className="hint" style={{ margin: '10px 0 8px' }}>按模型</div>
              <div className="tablewrap"><table><thead><tr><th>模型</th><th>调用</th><th>失败</th><th>成本（点）</th><th>输入</th><th>输出</th></tr></thead>
                <tbody>{(usage.by_model || []).map((x, i) => <tr key={i}><td>{x.name || x.id}</td><td className="num">{x.requests}</td><td className="num">{x.failed}</td><td className="num">{f2(x.cost)}</td><td className="num">{x.prompt}</td><td className="num">{x.completion}</td></tr>)}</tbody></table></div>
            </>
          )}
        </Card>
      )}
    </div>
  )
}