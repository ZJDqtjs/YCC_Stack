import React, { useEffect, useState } from 'react'
import { useApp } from '../ctx'
import { Btn, Card, Empty, Field, Loader, Sheet, Stats, Tag } from '../ui'
import { f1, f2, memGB } from '../../../shared/index.js'

function NodeCard({ w, onPrice, onPause, onOpen }: any) {
  const reliability = w.ok + w.bad > 0 ? Math.round((100 * w.ok) / (w.ok + w.bad)) + '%' : '—'
  const gpus = (w.gpus || []).map((g: any) => `${g.name} ${Math.round(g.mem_mb / 1024)}G`).join(' + ') || '无独显'
  return (
    <Card onClick={() => onOpen(w)}>
      <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        {w.name || w.id}
        <Tag tone={w.online ? 'ok' : 'bad'}>{w.online ? '在线' : '离线'}</Tag>
        {w.mine && <Tag tone="link">我的机器</Tag>}
        {w.paused && <Tag tone="bad">已停止派活</Tag>}
      </h3>
      <div className="hint" style={{ marginTop: 6 }}>
        {w.os} · {w.cpus} 核 · {memGB(w.mem_mb)} · {gpus}
      </div>
      <div className="tags">
        {(w.runtimes || []).map((r: any) => (
          <Tag key={r.name || r}>{r.name || r}</Tag>
        ))}
        {(w.encoders || []).map((e: any) => (
          <Tag key={e} tone="vio">
            {e}
          </Tag>
        ))}
      </div>
      <Stats
        items={[
          { v: `${w.active}/${w.slots}`, k: '占用槽位' },
          { v: w.speed > 0 ? f2(w.speed) + '×' : '—', k: '实测吞吐' },
          { v: reliability, k: `成功率 ✓${w.ok} ✗${w.bad}` },
          { v: f2(w.price) + '/秒', k: '单价', hl: w.mine },
        ]}
      />
      {w.parked_sec > 0 && <div className="hint warn" style={{ marginTop: 8 }}>连续失败，停派 {w.parked_sec}s</div>}
      {w.mine && (
        <div className="btnrow">
          <Btn variant="ghost" tiny onClick={(e: any) => { e.stopPropagation(); onPrice(w) }}>
            调整单价
          </Btn>
          <Btn variant="ghost" tiny onClick={(e: any) => { e.stopPropagation(); onPause(w, !w.paused) }}>
            {w.paused ? '恢复派活' : '暂停派活'}
          </Btn>
        </div>
      )}
    </Card>
  )
}

export default function Market() {
  const { api, workers, showToast, setTargetNode, setView } = useApp()
  const [buckets, setBuckets] = useState<any[] | null>(null)
  const [editing, setEditing] = useState<any>(null)
  const [price, setPrice] = useState('')
  const [detail, setDetail] = useState<any>(null)

  useEffect(() => {
    let alive = true
    const load = async () => {
      try {
        const d = await api.market()
        if (alive) setBuckets(d.slices || [])
      } catch {
        /* the state poll keeps the node list alive even if this fails */
      }
    }
    load()
    const t = setInterval(load, 5000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [api])

  const submitPrice = async () => {
    const n = Number(price)
    if (!(n > 0)) return showToast('请输入大于 0 的单价', 'bad')
    try {
      await api.setNodePrice(editing.id, n)
      showToast('已更新单价')
      setEditing(null)
    } catch (e: any) {
      showToast('改价失败：' + (e.message || e), 'bad')
    }
  }

  const pause = async (w: any, paused: boolean) => {
    try {
      await api.pauseNode(w.id, paused)
      showToast(paused ? '已停止派活' : '已恢复派活')
    } catch (e: any) {
      showToast('操作失败：' + (e.message || e), 'bad')
    }
  }

  const assign = () => {
    if (!detail) return
    setTargetNode(detail)
    setView('submit')
    setDetail(null)
    showToast('已指定节点：' + (detail.name || detail.id))
  }

  const online = workers.filter((w: any) => w.online)
  const mine = workers.filter((w: any) => w.mine)

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>算力市场</h1>
        <p>按编码能力聚合的切片与全部在线节点。他人机器只展示规格与价格，不展示主机名与地址。</p>
      </div>

      <Card kicker="Capacity" title="按能力聚合">
        {!buckets ? (
          <Loader />
        ) : buckets.length === 0 ? (
          <Empty>尚无在线算力</Empty>
        ) : (
          <div className="list">
            {buckets.map((b: any) => (
              <div className="listrow" key={b.encoder}>
                <div className="grow">
                  <div className="name mono">
                    {b.encoder} {/nvenc|qsv|amf/.test(b.encoder) && <Tag tone="vio">GPU</Tag>}
                  </div>
                  <div className="meta">
                    {b.nodes} 台 · 空闲 {b.free_slots} 槽 · {b.best_gpu || '无独显'} · 均速 {b.avg_speed > 0 ? f2(b.avg_speed) + '×' : '—'}
                  </div>
                </div>
                <div className="amount">
                  {b.min_price === b.max_price ? f2(b.min_price) : f2(b.min_price) + '~' + f2(b.max_price)}
                  <div className="meta" style={{ textAlign: 'right' }}>点/秒</div>
                </div>
              </div>
            ))}
          </div>
        )}
        <div className="hint" style={{ marginTop: 8 }}>一台机器可出现在多行；真实并发看「空闲槽位」。</div>
      </Card>

      <Card kicker="Mine" title={`我的在网节点（${mine.length}）`}>
        {mine.length === 0 ? <Empty>还没有属于你的节点</Empty> : mine.map((w: any) => <NodeCard key={w.id} w={w} onOpen={setDetail} onPrice={(n: any) => { setEditing(n); setPrice(String(n.price)) }} onPause={pause} />)}
      </Card>

      <Card kicker="Nodes" title={`全部在线节点（${online.length}）`}>
        {online.length === 0 ? <Empty>尚无节点注册</Empty> : online.map((w: any) => <NodeCard key={w.id} w={w} onOpen={setDetail} onPrice={(n: any) => { setEditing(n); setPrice(String(n.price)) }} onPause={pause} />)}
      </Card>

      <Sheet open={!!detail} title="节点详情" onClose={() => setDetail(null)}>
        {detail && (
          <>
            <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              {detail.name || detail.id}
              <Tag tone={detail.online ? 'ok' : 'bad'}>{detail.online ? '在线' : '离线'}</Tag>
              {detail.mine && <Tag tone="link">我的机器</Tag>}
            </h3>
            <div className="hint" style={{ marginTop: 6 }}>
              {detail.os} · {detail.cpus} 核 · {memGB(detail.mem_mb)} · 单价 {f2(detail.price)} 点/秒
            </div>
            <div className="tags">
              {(detail.runtimes || []).map((r: any) => <Tag key={r.name}>{r.name}</Tag>)}
              {(detail.encoders || []).map((e: any) => <Tag key={e} tone="vio">{e}</Tag>)}
            </div>
            <Stats
              items={[
                { v: `${detail.active}/${detail.slots}`, k: '占用槽位' },
                { v: detail.speed > 0 ? f2(detail.speed) + '×' : '—', k: '实测吞吐' },
                { v: detail.ok + detail.bad > 0 ? Math.round((100 * detail.ok) / (detail.ok + detail.bad)) + '%' : '—', k: `信誉 ✓${detail.ok} ✗${detail.bad}` },
              ]}
            />
            <div className="hint mono" style={{ marginTop: 8 }}>编号 {detail.id}</div>
            <div className="hint" style={{ marginTop: 6 }}>
              指定后，任务只会派给这台节点；它离线时会排队等待上线。
            </div>
            <div className="btnrow">
              <Btn block onClick={assign}>给这台节点派任务</Btn>
            </div>
          </>
        )}
      </Sheet>

      <Sheet open={!!editing} title="调整单价" onClose={() => setEditing(null)}>
        <div className="hint">{editing?.name || editing?.id} 当前 {f2(editing?.price)} 点/视频秒</div>
        <Field label="新单价（算力点 / 视频秒）">
          <input className="in" type="number" min="0.05" step="0.05" value={price} onChange={(e) => setPrice(e.target.value)} />
        </Field>
        <div className="btnrow">
          <Btn block onClick={submitPrice}>
            保存
          </Btn>
        </div>
      </Sheet>
    </div>
  )
}
