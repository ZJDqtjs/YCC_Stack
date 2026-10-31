import React, { useEffect, useState } from 'react'
import { openExternal } from '../api'
import { useApp } from '../ctx'
import { Btn, Card, CopyBlock, Empty, Field, Loader, Sheet, Stats, Tag } from '../ui'
import { f1, f2, memGB } from '../../../shared/index.js'

const ua = (navigator.userAgent || '').toLowerCase()
const guessOS = () => (ua.includes('windows') ? 'windows' : ua.includes('mac') ? 'darwin' : 'linux')
const guessArch = () => (/arm|aarch64/.test(ua) ? 'arm64' : 'amd64')

export default function Nodes() {
  const { api, workers, policy, showToast } = useApp()
  const [f, setF] = useState({ name: '', price: '0.80', slots: '2', os: guessOS(), arch: guessArch() })
  const [msg, setMsg] = useState('')
  const [scriptUrl, setScriptUrl] = useState('')
  const [busy, setBusy] = useState(false)
  const [pending, setPending] = useState<any[] | null>(null)
  const [code, setCode] = useState('')
  const [editing, setEditing] = useState<any>(null)
  const [price, setPrice] = useState('')
  const set = (k: string, v: any) => setF((prev) => ({ ...prev, [k]: v }))

  const mine = (workers || []).filter((w: any) => w.mine)

  useEffect(() => {
    if (!policy?.pending) return
    api
      .pending()
      .then((d: any) => setPending(d.pending || []))
      .catch(() => setPending([]))
  }, [api, policy])

  const launch = async () => {
    const body = { name: f.name.trim() || '我的节点', price: Number(f.price), slots: Number(f.slots), os: f.os, arch: f.arch }
    if (!(body.price >= 0.05)) return showToast('单价至少 0.05 点/视频秒', 'bad')
    if (!(body.slots >= 1)) return showToast('并发槽至少 1 个', 'bad')
    setBusy(true)
    setMsg('正在生成启动码…')
    try {
      const d = await api.launchNode(body)
      openExternal(api.launchScriptUrl(d.bundle || d.script))
      setScriptUrl(api.launchScriptUrl(d.script))
      const mins = Math.max(1, Math.round(d.expires_in_sec / 60))
      setMsg(`节点包已开始下载（${d.node_id}，${mins} 分钟内有效）。解压后直接双击 starstack-node.exe 即可加入，配置在包里，不需要 bat；关掉窗口即停止。${d.ready ? '' : ' 注意：Broker 上还没有对应平台的 Agent 程序。'}`)
      showToast('启动脚本已生成')
    } catch (e: any) {
      setMsg('生成失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const claim = async () => {
    const c = code.trim().toUpperCase()
    if (!c) return
    try {
      const r = await api.pair(c)
      setCode('')
      showToast('已认领节点 ' + r.node_id)
    } catch (e: any) {
      showToast('认领失败：' + (e.message || e), 'bad')
    }
  }

  const savePrice = async () => {
    const n = Number(price)
    if (!(n > 0)) return showToast('请输入大于 0 的单价', 'bad')
    try {
      await api.setNodePrice(editing.id, n)
      setEditing(null)
      showToast('已更新单价')
    } catch (e: any) {
      showToast('改价失败：' + (e.message || e), 'bad')
    }
  }

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>我的节点</h1>
        <p>把闲置机器接进来赚算力点：下载节点包，解压后直接双击 exe；启动脚本作为备选。</p>
      </div>

      <Card kicker="Provider" title="一键提供算力">
        <Field label="机器名称">
          <input className="in" value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="例如 宿舍的台式机" />
        </Field>
        <div className="row">
          <Field label="单价（点/视频秒）">
            <input className="in" type="number" min="0.05" step="0.05" value={f.price} onChange={(e) => set('price', e.target.value)} />
          </Field>
          <Field label="并发槽">
            <input className="in" type="number" min="1" value={f.slots} onChange={(e) => set('slots', e.target.value)} />
          </Field>
        </div>
        <div className="row">
          <Field label="那台机器的系统">
            <select className="in" value={f.os} onChange={(e) => set('os', e.target.value)}>
              <option value="windows">Windows</option>
              <option value="linux">Linux</option>
              <option value="darwin">macOS</option>
            </select>
          </Field>
          <Field label="芯片">
            <select className="in" value={f.arch} onChange={(e) => set('arch', e.target.value)}>
              <option value="amd64">x86_64</option>
              <option value="arm64">arm64</option>
            </select>
          </Field>
        </div>
        <div className="btnrow">
          <Btn block onClick={launch} disabled={busy}>
            {busy ? <span className="spin" /> : null} 下载节点包（exe + 配置）
          </Btn>
        </div>
        {msg && <div className="hint" style={{ marginTop: 8 }}>{msg}</div>}
        {scriptUrl && (
          <div className="btnrow">
            <Btn variant="ghost" tiny onClick={() => openExternal(scriptUrl)}>
              启动脚本（备选）
            </Btn>
          </div>
        )}
        <div className="hint" style={{ marginTop: 10 }}>
          脚本一次有效、30 分钟内可用；Agent 会先检查 ffmpeg/blender 等运行时，再以你的账户身份加入算力池。
        </div>
      </Card>

      <Card kicker="Manual" title="手动运行 Agent">
        <CopyBlock
          text={`starstack-worker --server ${api.baseUrl()} \\\n  --name ${f.name || '我的旧台式机'} --price ${f.price}${policy?.public ? ' \\\n  --token <运维分配的接入令牌>' : ''}`}
        />
        <Field label="配对码" hint="Agent 启动后会打印一次性配对码，输入即可把机器挂到你的账户下。">
          <div className="row">
            <input className="in" value={code} onChange={(e) => setCode(e.target.value)} placeholder="例如 A1B2C3" />
            <Btn variant="ghost" onClick={claim}>
              认领
            </Btn>
          </div>
        </Field>
      </Card>

      {policy?.pending && (
        <Card kicker="Pending" title="待认领的 Agent">
          {!pending ? (
            <Loader />
          ) : pending.length === 0 ? (
            <Empty>没有待认领的机器</Empty>
          ) : (
            <div className="list">
              {pending.map((x: any) => (
                <div className="listrow" key={x.id}>
                  <div className="grow">
                    <div className="name">{x.name || x.id}</div>
                    <div className="meta">
                      {x.os} · {x.cpus} 核 · {f2(x.price)} 点/秒
                    </div>
                    <div className="tags">
                      <Tag tone={x.online ? 'ok' : 'bad'}>{x.online ? '在线' : '离线'}</Tag>
                      <Tag>{x.code}</Tag>
                    </div>
                  </div>
                  <Btn variant="ghost" tiny onClick={() => api.pair(x.code).then(() => showToast('已认领 ' + x.id)).catch((e: any) => showToast('认领失败：' + e.message, 'bad'))}>
                    认领
                  </Btn>
                </div>
              ))}
            </div>
          )}
        </Card>
      )}

      <Card kicker="Mine" title={`我的机器（${mine.length}）`}>
        {mine.length === 0 ? (
          <Empty>还没有属于你的节点</Empty>
        ) : (
          mine.map((w: any) => (
            <div key={w.id} style={{ paddingTop: 10 }}>
              <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                {w.name || w.id}
                <Tag tone={w.online ? 'ok' : 'bad'}>{w.online ? '在线' : '离线'}</Tag>
                {w.paused && <Tag tone="warn">已暂停</Tag>}
              </h3>
              <Stats
                items={[
                  { v: f2(w.earned), k: '累计收益' },
                  { v: f1(w.seconds_billed) + 's', k: '已结算算力' },
                  { v: `${w.active}/${w.slots}`, k: '占用槽位' },
                  { v: f2(w.price), k: '当前单价' },
                  { v: w.speed > 0 ? f2(w.speed) + '×' : '—', k: '实测吞吐' },
                  { v: `${w.ok}/${w.ok + w.bad}`, k: '成功/总数' },
                ]}
              />
              <div className="hint" style={{ marginTop: 8 }}>
                {w.hostname || ''} · {w.os} · {w.cpus} 核 · {memGB(w.mem_mb)} · 空闲磁盘 {w.disk_free_mb ? memGB(w.disk_free_mb) : '未知'}
              </div>
              <div className="btnrow">
                <Btn variant="ghost" tiny onClick={() => { setEditing(w); setPrice(String(w.price)) }}>
                  调整单价
                </Btn>
                <Btn
                  variant="ghost"
                  tiny
                  onClick={() =>
                    api
                      .pauseNode(w.id, !w.paused)
                      .then(() => showToast(w.paused ? '已恢复派活' : '已暂停派活'))
                      .catch((e: any) => showToast('操作失败：' + e.message, 'bad'))
                  }
                >
                  {w.paused ? '恢复派活' : '暂停派活'}
                </Btn>
              </div>
              <div className="hint mono" style={{ marginTop: 8 }}>编号 {w.id}</div>
            </div>
          ))
        )}
      </Card>

      <Sheet open={!!editing} title="调整单价" onClose={() => setEditing(null)}>
        <div className="hint">{editing?.name || editing?.id} 当前 {f2(editing?.price)} 点/视频秒</div>
        <Field label="新单价（算力点 / 视频秒）">
          <input className="in" type="number" min="0.05" step="0.05" value={price} onChange={(e) => setPrice(e.target.value)} />
        </Field>
        <div className="btnrow">
          <Btn block onClick={savePrice}>
            保存
          </Btn>
        </div>
      </Sheet>
    </div>
  )
}
