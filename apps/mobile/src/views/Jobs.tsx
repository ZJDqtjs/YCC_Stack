import React, { useEffect, useMemo, useState } from 'react'
import { downloadArtifact } from '../api'
import { useApp } from '../ctx'
import { Bar, Btn, Card, Empty, Loader, Stats, Tag } from '../ui'
import { f1, f2, shortId } from '../../../shared/index.js'
import { INFLIGHT, JOB_ST, toneOf } from '../../../shared/constants.js'

function Segment({ s, kind }: any) {
  const cls = s.status === 'done' ? 'done' : s.status === 'failed' ? 'failed' : INFLIGHT.includes(s.status) ? 'running' : 'pending'
  const h = Math.max(2, Math.min(100, s.status === 'done' ? 100 : s.pct))
  const blender = kind === 'blender'
  const range = blender ? `#${s.index} 帧 ${Math.round(s.start)}~${Math.round(s.start + s.dur - 1)}` : `#${s.index} ${f1(s.start)}→${f1(s.start + s.dur)}s`
  const who = (s.worker || '—').slice(0, 8)
  return (
    <div className={'pc ' + cls} title={`${range} · ${who} · ${f2(s.cost)} 点`}>
      <div className="fill" style={{ height: h + '%' }} />
      <div className="lbl">
        #{s.index}
        <br />
        {who}
      </div>
    </div>
  )
}

function JobCard({ j }: any) {
  const { api, showToast } = useApp()
  const segs = j.segs || []
  const done = segs.filter((s: any) => s.status === 'done').length
  const verified = segs.filter((s: any) => s.match).length
  const progress = segs.length ? (done / segs.length) * 100 : 0
  const speedup = !j.resumed && j.wall > 0 ? j.sum_wall / j.wall : 0
  const real = !j.resumed && j.baseline && j.baseline.status === 'done' && j.wall > 0 ? j.baseline.wall / j.wall : 0
  const rq: string[] = []
  const req = j.req || {}
  if (req.need_gpu) rq.push('需 GPU')
  if (req.min_cpus) rq.push('≥' + req.min_cpus + ' 核')
  if (req.min_gpu_mem_mb) rq.push('显存≥' + req.min_gpu_mem_mb + 'MB')
  if (req.runtime) rq.push('需 ' + req.runtime)
  if (req.max_price) rq.push('≤' + f2(req.max_price) + ' 点/秒')

  const save = async (jobId: string, which: string) => {
    const ext = j.kind === 'build' || j.kind === 'blender' ? 'zip' : j.kind === 'pr' ? 'mkv' : 'mp4'
    const name = String(j.name || jobId).replace(/[\/:*?"<>|]/g, '_') + (which ? '-' + which : '') + '.' + ext
    try {
      await downloadArtifact(api.downloadUrl(jobId, which), name)
      showToast('已保存到下载目录')
    } catch (e: any) {
      showToast('下载失败：' + (e.message || e), 'bad')
    }
  }

  const profile = j.profile || {}
  const statusLabel = j.kind === 'build' && j.status === 'running' ? '编译中' : JOB_ST[j.status] || j.status
  const specTags =
    j.kind === 'build' ? (
      <>
        <Tag>编译打包</Tag>
        <Tag>{String((j.spec && j.spec.command) || '').slice(0, 40)}</Tag>
        <Tag>超时 {j.spec && j.spec.timeout_sec ? j.spec.timeout_sec : 1800}s</Tag>
      </>
    ) : j.kind === 'blender' ? (
      <>
        <Tag>Blender{j.spec && j.spec.engine ? ' · ' + j.spec.engine : ''}{j.spec && j.spec.samples ? ' · ' + j.spec.samples + ' spp' : ''}</Tag>
        <Tag>{Math.round(j.duration)} 帧 · {segs.length} 段</Tag>
      </>
    ) : j.kind === 'pr' ? (
      <>
        <Tag>时间线渲染</Tag>
        <Tag>{f1(j.duration)}s · {segs.length} 片</Tag>
      </>
    ) : (
      <>
        <Tag>{profile.vcodec} · Q{profile.q} · {profile.preset}</Tag>
        <Tag>{profile.height ? profile.height + 'p' : '原始分辨率'}</Tag>
        <Tag>{f1(j.duration)}s · {segs.length} 片</Tag>
      </>
    )

  const stats: { v: any; k: string; hl?: boolean }[] = [
    { v: f1(j.wall) + 's', k: '分布式墙钟' },
    { v: f1(j.sum_wall) + 's', k: '分片耗时和' },
    { v: speedup > 0 ? f2(speedup) + 'x' : '—', k: '并行比' },
    { v: done + '/' + segs.length, k: '分片完成' },
    { v: verified + '/' + segs.length, k: '哈希一致' },
    { v: f2(j.credits_spent), k: '实结算' },
  ]
  const buildStats = [
    { v: f1(j.wall) + 's', k: '编译耗时' },
    { v: verified + '/' + segs.length, k: '产物校验' },
    { v: f2(j.credits_spent), k: '实结算' },
  ]
  if (j.released && j.credits_hold > j.credits_spent) {
    const refund = { v: '+' + f2(j.credits_hold - j.credits_spent), k: '退还' }
    stats.push(refund)
    buildStats.push(refund)
  }
  if (j.baseline) stats.push({ v: j.baseline.status === 'done' ? f1(j.baseline.wall) + 's' : j.baseline.status === 'running' ? '运行中' : '排队', k: '单机基线' })
  if (real > 0) stats.push({ v: f2(real) + 'x', k: '真实加速比', hl: true })

  return (
    <Card>
      <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        {j.name}
        <Tag tone={toneOf(j.status)}>{statusLabel}</Tag>
      </h3>
      <div className="tags">
        {specTags}
        <Tag>预扣 {f2(j.credits_hold)} 点</Tag>
        {j.resumed && <Tag tone="vio">跨重启恢复</Tag>}
        {rq.length > 0 && <Tag tone="vio">硬约束：{rq.join(' / ')}</Tag>}
      </div>
      {j.note && <div className="hint" style={{ marginTop: 8 }}>{j.note}</div>}
      <div className="prog">
        <Bar pct={progress} />
        <span className="sub">{done}/{segs.length}</span>
      </div>
      {segs.length > 0 && (
        <div className="pieces">
          {segs.map((s: any) => (
            <Segment key={s.id || s.index} s={s} kind={j.kind} />
          ))}
        </div>
      )}
      <Stats items={j.kind === 'build' ? buildStats : stats} />
      {j.err && <div className="err" style={{ marginTop: 10 }}>错误：{j.err}</div>}
      <div className="btnrow">
        {(j.status === 'done' || (j.baseline && j.baseline.status === 'done')) && (
          <>
            {j.status === 'done' && (
              <Btn variant="ghost" tiny onClick={() => save(j.id, '')}>
                下载产物
              </Btn>
            )}
            {j.baseline && j.baseline.status === 'done' && (
              <Btn variant="ghost" tiny onClick={() => save(j.id, 'baseline')}>
                下载基线
              </Btn>
            )}
          </>
        )}
        <span className="hint mono" style={{ alignSelf: 'center' }}>{shortId(j.id, 12)}</span>
      </div>
    </Card>
  )
}

export default function Jobs() {
  const { api } = useApp()
  const [list, setList] = useState<any[] | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let alive = true
    const load = async () => {
      try {
        const d = await api.jobs()
        if (alive) {
          setList(d.jobs || [])
          setErr('')
        }
      } catch (e: any) {
        if (alive) setErr(e.message || String(e))
      }
    }
    load()
    const t = setInterval(load, 3000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [api])

  const body = useMemo(() => {
    if (!list) return <Loader />
    if (err) return <Card><div className="err">{err}</div></Card>
    if (!list.length) return <Card><Empty>还没有任务</Empty></Card>
    return list.map((j) => <JobCard key={j.id} j={j} />)
  }, [list, err])

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>我的任务</h1>
        <p>分片去向、进度、哈希校验与结算都在这里；跨 Broker 重启恢复的任务不计量加速比。</p>
      </div>
      {body}
    </div>
  )
}
