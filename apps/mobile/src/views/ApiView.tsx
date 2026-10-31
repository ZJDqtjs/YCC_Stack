import React, { useEffect, useMemo, useState } from 'react'
import { useApp } from '../ctx'
import { Btn, Card, CopyBlock, Empty, Field, Loader, Stats, Tag } from '../ui'
import { ctxLabel, f2, ts } from '../../../shared/index.js'
import { CAP_NAMES } from '../../../shared/constants.js'

const CAPS = Object.keys(CAP_NAMES)

export default function ApiView() {
  const { api, showToast } = useApp()
  const [keys, setKeys] = useState<any[] | null>(null)
  const [ups, setUps] = useState<any[] | null>(null)
  const [cat, setCat] = useState<any[] | null>(null)
  const [keyName, setKeyName] = useState('')
  const [newKey, setNewKey] = useState('')
  const [model, setModel] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [showAdd, setShowAdd] = useState(false)
  const [form, setForm] = useState<any>({
    name: '',
    base_url: '',
    api_key: '',
    models: '',
    price: '0.5',
    price_out: '0',
    price_cached: '0',
    max_context: '32768',
    caps: [],
  })
  const set = (k: string, v: any) => setForm((p: any) => ({ ...p, [k]: v }))

  const load = async () => {
    try {
      const [k, u, c] = await Promise.all([api.keys(), api.upstreams(), api.catalog()])
      setKeys(k.keys || [])
      setUps(u.upstreams || [])
      const models = c.models || []
      setCat(models)
      setModel((cur) => (cur && models.some((m: any) => m.id === cur) ? cur : models[0]?.id || ''))
    } catch (e: any) {
      showToast('读取失败：' + (e.message || e), 'bad')
      setKeys([])
      setUps([])
      setCat([])
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const createKey = async () => {
    setBusy(true)
    try {
      const d = await api.createKey(keyName.trim() || '默认密钥')
      setNewKey(d.key)
      setKeyName('')
      await load()
    } catch (e: any) {
      showToast('创建失败：' + (e.message || e), 'bad')
    } finally {
      setBusy(false)
    }
  }

  const revoke = async (id: string) => {
    try {
      await api.revokeKey(id)
      await load()
      showToast('已撤销')
    } catch (e: any) {
      showToast('撤销失败：' + (e.message || e), 'bad')
    }
  }

  const probe = async () => {
    setMsg('正在请求上游 /v1/models…')
    try {
      const d = await api.probeUpstream(form.base_url.trim(), form.api_key)
      setMsg(`探测成功（${d.latency_ms} ms）：${(d.models || []).join('、')}`)
      if (!form.models.trim()) set('models', (d.models || []).slice(0, 8).join(', '))
    } catch (e: any) {
      setMsg('探测失败：' + (e.message || e))
    }
  }

  const addUpstream = async () => {
    const body = {
      name: form.name.trim(),
      base_url: form.base_url.trim(),
      api_key: form.api_key,
      models: form.models.trim(),
      price: Number(form.price),
      price_out: Number(form.price_out) || 0,
      price_cached: Number(form.price_cached) || 0,
      max_context: Number(form.max_context) || 32768,
      caps: form.caps.join(','),
    }
    if (!body.base_url) return setMsg('请填写 Base URL')
    if (!body.models) return setMsg('请填写至少一个模型名')
    if (!(body.price > 0)) return setMsg('输入单价必须大于 0')
    if (body.max_context < 1024) return setMsg('最大上下文至少 1024 tokens')
    setBusy(true)
    setMsg('正在登记…')
    try {
      await api.addUpstream(body)
      setMsg('已登记，模型已进入可用目录')
      setForm((p: any) => ({ ...p, name: '', base_url: '', api_key: '', models: '', price_out: '0', price_cached: '0', max_context: '32768', caps: [] }))
      await load()
    } catch (e: any) {
      setMsg('登记失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const base = api.baseUrl() + '/v1'
  const snippet = useMemo(
    () =>
      `curl ${api.baseUrl()}/v1/chat/completions \\\n  -H "Authorization: Bearer ${newKey || 'sk-ss-...'}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model || 'your-model'}","messages":[{"role":"user","content":"你好"}]}'`,
    [api, newKey, model],
  )

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>模型 API</h1>
        <p>用 OpenAI 兼容接口调用平台聚合的算力；登记 llama.cpp / vLLM / Ollama 端点，按 token 结算。</p>
      </div>

      <Card kicker="Keys" title="我的 API Key">
        <div className="row">
          <input className="in" value={keyName} onChange={(e) => setKeyName(e.target.value)} placeholder="密钥名称，例如 论文实验" />
          <Btn onClick={createKey} disabled={busy}>
            创建
          </Btn>
        </div>
        {newKey && (
          <div style={{ marginTop: 10 }}>
            <div className="hint warn">新密钥只显示这一次，请立即复制保存。</div>
            <CopyBlock text={newKey} label="复制密钥" />
          </div>
        )}
        {!keys ? (
          <Loader />
        ) : keys.length === 0 ? (
          <Empty>还没有 API Key</Empty>
        ) : (
          <div className="list" style={{ marginTop: 8 }}>
            {keys.map((k: any) => (
              <div className="listrow" key={k.id}>
                <div className="grow">
                  <div className="name">
                    {k.name} {k.revoked && <Tag tone="bad">已撤销</Tag>}
                  </div>
                  <div className="meta mono">
                    {k.prefix} · 创建 {ts(k.created_at)} · 最近 {k.last_used ? ts(k.last_used) : '—'}
                  </div>
                </div>
                {!k.revoked && (
                  <Btn variant="ghost" tiny onClick={() => revoke(k.id)}>
                    撤销
                  </Btn>
                )}
              </div>
            ))}
          </div>
        )}
      </Card>

      <Card kicker="Upstream" title="接入模型上游">
        <div className="hint">llama.cpp / vLLM / Ollama 的 OpenAI 兼容端点都能接入。MoE 可用多台机器的 rpc-server 聚合成一个端点，平台只感知这个端点。</div>
        <div className="btnrow">
          <Btn variant="ghost" tiny onClick={() => setShowAdd((v) => !v)}>
            {showAdd ? '收起表单' : '登记新上游'}
          </Btn>
        </div>
        {showAdd && (
          <>
            <Field label="上游名称">
              <input className="in" value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="例如 宿舍 4090 · Qwen3" />
            </Field>
            <Field label="Base URL">
              <input className="in" value={form.base_url} onChange={(e) => set('base_url', e.target.value)} placeholder="http://192.168.1.20:8080" />
            </Field>
            <div className="row">
              <Field label="上游 API Key（可选）">
                <input className="in" type="password" value={form.api_key} onChange={(e) => set('api_key', e.target.value)} />
              </Field>
              <Field label="输入单价（点/千 token）">
                <input className="in" type="number" min="0.0001" step="0.1" value={form.price} onChange={(e) => set('price', e.target.value)} />
              </Field>
            </div>
            <div className="row">
              <Field label="输出单价（0=同输入）">
                <input className="in" type="number" min="0" step="0.1" value={form.price_out} onChange={(e) => set('price_out', e.target.value)} />
              </Field>
              <Field label="缓存单价（0=同输入）">
                <input className="in" type="number" min="0" step="0.1" value={form.price_cached} onChange={(e) => set('price_cached', e.target.value)} />
              </Field>
            </div>
            <Field label="最大上下文（tokens）">
              <input className="in" type="number" min="1024" step="1024" value={form.max_context} onChange={(e) => set('max_context', e.target.value)} />
            </Field>
            <div className="field">
              <span>声明能力</span>
              <div className="tags">
                {CAPS.map((c) => {
                  const on = form.caps.includes(c)
                  return (
                    <button key={c} className={'tag' + (on ? ' link' : '')} onClick={() => set('caps', on ? form.caps.filter((x: string) => x !== c) : [...form.caps, c])}>
                      {CAP_NAMES[c]}
                    </button>
                  )
                })}
              </div>
            </div>
            <Field label="提供的模型名（逗号分隔）">
              <div className="row">
                <input className="in" value={form.models} onChange={(e) => set('models', e.target.value)} placeholder="qwen3-30b-a3b, qwen2.5-7b" />
                <Btn variant="ghost" onClick={probe}>
                  探测
                </Btn>
              </div>
            </Field>
            {msg && <div className="hint" style={{ marginTop: 8 }}>{msg}</div>}
            <div className="btnrow">
              <Btn block onClick={addUpstream} disabled={busy}>
                登记上游
              </Btn>
            </div>
          </>
        )}
      </Card>

      <Card kicker="Registered" title="我登记的上游">
        {!ups ? (
          <Loader />
        ) : ups.length === 0 ? (
          <Empty>还没有登记上游</Empty>
        ) : (
          ups.map((u: any) => {
            const h = u.health || {}
            const caps = (u.caps || []).filter((c: string) => c !== 'chat')
            return (
              <div key={u.id} style={{ paddingTop: 12 }}>
                <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  {u.name}
                  {u.enabled ? <Tag tone="ok">{h.cooling_sec ? `冷却 ${h.cooling_sec}s` : '路由中'}</Tag> : <Tag tone="bad">已停用</Tag>}
                </h3>
                <div className="hint" style={{ marginTop: 4 }}>
                  {u.base_url} · {(u.models || []).join(' / ')}
                </div>
                <div className="hint">上下文 {ctxLabel(u.max_context)} · {caps.length ? caps.map((c: string) => CAP_NAMES[c] || c).join(' / ') : '仅对话'}</div>
                <Stats
                  items={[
                    { v: f2(u.price), k: '输入/千 token' },
                    { v: f2(u.price_out || u.price), k: '输出' },
                    { v: f2(u.price_cached || u.price), k: '缓存' },
                    { v: h.last_ok ? ts(h.last_ok).slice(-8) : '—', k: '最近成功' },
                  ]}
                />
                {h.last_error && <div className="hint warn" style={{ marginTop: 6 }}>最近错误：{h.last_error}</div>}
                <div className="btnrow">
                  <Btn variant="ghost" tiny onClick={() => api.setUpstream(u.id, !u.enabled).then(load).catch((e: any) => showToast('操作失败：' + e.message, 'bad'))}>
                    {u.enabled ? '停用' : '启用'}
                  </Btn>
                  <Btn variant="ghost" tiny onClick={() => api.delUpstream(u.id).then(load).catch((e: any) => showToast('删除失败：' + e.message, 'bad'))}>
                    删除
                  </Btn>
                </div>
              </div>
            )
          })
        )}
      </Card>

      <Card kicker="Catalog" title="可用模型">
        {!cat ? (
          <Loader />
        ) : cat.length === 0 ? (
          <Empty>还没有可用模型</Empty>
        ) : (
          <div className="list">
            {cat.map((m: any) => (
              <div className="listrow" key={m.id}>
                <div className="grow">
                  <div className="name mono">{m.id}</div>
                  <div className="meta">
                    {m.providers} 个上游 · {ctxLabel(m.max_context)} 上下文
                  </div>
                  <div className="tags">
                    {(m.capabilities || []).filter((c: string) => c !== 'chat').map((c: string) => (
                      <Tag key={c}>{CAP_NAMES[c] || c}</Tag>
                    ))}
                  </div>
                </div>
                <div className="amount">
                  {f2(m.price_per_1k)}
                  <div className="meta" style={{ textAlign: 'right' }}>点/千 token</div>
                </div>
              </div>
            ))}
          </div>
        )}
        <div className="hint" style={{ marginTop: 8 }}>按上下文与能力过滤后挑最便宜的健康上游；余额不足返回 402。</div>
      </Card>

      <Card kicker="Example" title="调用示例">
        <Field label="模型">
          <select className="in" value={model} onChange={(e) => setModel(e.target.value)}>
            {!cat || cat.length === 0 ? (
              <option value="">（先登记一个模型）</option>
            ) : (
              cat.map((m: any) => (
                <option key={m.id} value={m.id}>
                  {m.id} · {f2(m.price_per_1k)} 点/千 token
                </option>
              ))
            )}
          </select>
        </Field>
        <CopyBlock text={snippet} label="复制 curl" />
        <div className="hint" style={{ marginTop: 8 }}>
          任意 OpenAI SDK 把 base_url 指到 <code>{base}</code> 即可。
        </div>
      </Card>
    </div>
  )
}
