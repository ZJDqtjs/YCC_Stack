import React, { useEffect, useMemo, useRef, useState } from 'react'
import { useApp } from '../ctx'
import { Btn, Card, Field, Stats, Tag } from '../ui'
import { f1, f2 } from '../../../shared/index.js'
import { QUALITY } from '../../../shared/constants.js'

const DEFAULTS = {
  segs: '4',
  path: '',
  vcodec: 'libx264',
  preset: 'fast',
  q: '26',
  height: '0',
  audio: '1',
  need_gpu: '0',
  min_gpu_mem: '0',
  min_cpus: '0',
  min_mem: '0',
  runtime: '',
  arch: '',
  max_price: '0',
  duration: '0',
  baseline: true,
  scene: '',
  engine: '',
  samples: '0',
  from: '1',
  to: '120',
  percent: '0',
  fps: '24',
  command: '',
  output: 'out',
  timeout: '1800',
}

export default function Submit() {
  const { api, policy, setView, showToast, targetNode, setTargetNode } = useApp()
  const [kind, setKind] = useState('transcode')
  const [quality, setQuality] = useState('balanced')
  const [adv, setAdv] = useState(false)
  const [f, setF] = useState<any>({ ...DEFAULTS })
  const [file, setFile] = useState<File | null>(null)
  const [quote, setQuote] = useState<any>(null)
  const [quoteErr, setQuoteErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const timer = useRef<any>(null)

  const set = (k: string, v: any) => setF((prev: any) => ({ ...prev, [k]: v }))

  useEffect(() => {
    api
      .settings()
      .then((d: any) => setAdv(!!d.advanced))
      .catch(() => {})
  }, [api])

  const applyQuality = (q: string) => {
    const preset = QUALITY[q] || QUALITY.balanced
    setF((prev: any) => ({ ...prev, ...preset }))
  }

  useEffect(() => {
    if (!adv && kind === 'transcode') applyQuality(quality)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [quality, adv, kind])

  const isRender = kind !== 'transcode'
  const units = useMemo(() => {
    if (kind === 'blender') {
      const a = Number(f.from || 0)
      const b = Number(f.to || 0)
      return b - a + 1
    }
    return 0
  }, [kind, f.from, f.to])

  const req = () => {
    const r: any = {}
    if (f.need_gpu === '1') r.need_gpu = true
    if (Number(f.min_gpu_mem) > 0) r.min_gpu_mem_mb = Number(f.min_gpu_mem)
    if (Number(f.min_cpus) > 0) r.min_cpus = Number(f.min_cpus)
    if (Number(f.min_mem) > 0) r.min_mem_mb = Number(f.min_mem)
    if (Number(f.max_price) > 0) r.max_price = Number(f.max_price)
    if (f.runtime) r.runtime = f.runtime
    if (f.arch) r.arch = f.arch
    if (targetNode) r.node_id = targetNode.id
    return r
  }

  // Quote is re-asked on every meaningful change, debounced like the console.
  useEffect(() => {
    clearTimeout(timer.current)
    timer.current = setTimeout(async () => {
      if (kind === 'build') {
        setQuote(null)
        setQuoteErr('编译任务按实际墙钟分钟结算，预扣 10 分钟；只派给开启 --allow-build 的节点。')
        return
      }
      try {
        let duration = Number(f.duration || 0)
        if (isRender) duration = kind === 'blender' ? units : 0
        const d = kind === 'blender' ? units : kind === 'pr' ? 1 : 0
        if (!(duration > 0)) {
          setQuote(null)
          setQuoteErr(kind === 'blender' ? '填写起止帧后按帧数估算' : kind === 'pr' ? '时间线时长由工程决定，按每秒报价' : '填视频时长或选择文件后自动估算')
          return
        }
        const q = isRender ? await api.estimate({ duration: d, kind, vcodec: 'libx264', req: req() }) : await api.estimate({ duration, vcodec: f.vcodec, req: req() })
        setQuote(q)
        setQuoteErr('')
      } catch (e: any) {
        setQuote(null)
        setQuoteErr('估算失败：' + (e.message || e))
      }
    }, 320)
    return () => clearTimeout(timer.current)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, f.duration, f.vcodec, f.segs, f.need_gpu, f.min_gpu_mem, f.min_cpus, f.min_mem, f.max_price, f.runtime, f.arch, units])

  const pickFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const picked = e.target.files && e.target.files[0] ? e.target.files[0] : null
    setFile(picked)
    if (picked && kind === 'transcode') {
      const v = document.createElement('video')
      v.preload = 'metadata'
      v.onloadedmetadata = () => {
        if (Number.isFinite(v.duration) && v.duration > 0) set('duration', String(Math.round(v.duration)))
      }
      v.src = URL.createObjectURL(picked)
    }
  }

  const submit = async () => {
    if (!file && !(kind === 'transcode' && policy?.local_path && f.path)) {
      return showToast('请选择文件', 'bad')
    }
    setBusy(true)
    setMsg('上传并登记任务中…')
    try {
      if (kind === 'build') {
        if (!String(f.command || '').trim()) {
          setBusy(false)
          setMsg('')
          return showToast('请填写编译命令', 'bad')
        }
        const fd = new FormData()
        fd.set('req', JSON.stringify(req()))
        fd.set('file', file!)
        fd.set('command', String(f.command).trim())
        fd.set('output', String(f.output || '').trim())
        fd.set('timeout', String(f.timeout || '1800'))
        const r = await api.submitBuild(fd)
        setMsg(`已入队 ${r.id}，预扣 ${f2(r.hold)} 点（按实际分钟结算）`)
        setTargetNode(null)
        showToast('编译任务已提交')
        setTimeout(() => setView('jobs'), 500)
        return
      }
      const fd = new FormData()
      // Common fields first; the server ignores the ones a kind does not use.
      fd.set('req', JSON.stringify(req()))
      fd.set('segs', String(f.segs || 4))
      if (file) fd.set('file', file)
      if (kind === 'transcode') {
        if (policy?.local_path && f.path) fd.set('path', f.path)
        fd.set('vcodec', f.vcodec)
        fd.set('preset', f.preset)
        fd.set('q', f.q)
        fd.set('height', f.height)
        fd.set('audio', f.audio)
        fd.set('baseline', f.baseline ? '1' : '0')
        const r = await api.submitTranscode(fd)
        setMsg(`已入队 ${r.id}，预扣 ${f2(r.hold)} 点`)
      } else {
        fd.set('kind', kind)
        if (kind === 'blender') {
          fd.set('scene', f.scene)
          fd.set('engine', f.engine)
          fd.set('samples', f.samples)
          fd.set('from', f.from)
          fd.set('to', f.to)
          fd.set('percent', f.percent)
          fd.set('fps', f.fps)
        }
        const r = await api.submitRender(fd)
        setMsg(`已入队 ${r.id}（${r.pieces} 段 / ${f1(r.units)} ${r.unit}），预扣 ${f2(r.hold)} 点`)
      }
      setTargetNode(null)
      showToast('任务已提交')
      setTimeout(() => setView('jobs'), 500)
    } catch (e: any) {
      setMsg('失败：' + (e.message || e))
      showToast('提交失败：' + (e.message || e), 'bad')
    } finally {
      setBusy(false)
    }
  }

  const ok = quote && quote.affordable && quote.eligible > 0

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>提交任务</h1>
        <p>转码按视频秒计价，渲染按帧计价；提交即预扣，完成后按实际用量结算并退还差额。</p>
      </div>

      {targetNode && (
        <Card kicker="Target" title="已指定节点">
          <div className="hint">
            本次任务只会派发给 <b>{targetNode.name || targetNode.id}</b>（{targetNode.id}）。它离线时会排队等待上线。
          </div>
          <div className="btnrow">
            <Btn variant="ghost" tiny onClick={() => setTargetNode(null)}>
              取消指定
            </Btn>
          </div>
        </Card>
      )}

      <Card kicker="Job" title="任务与文件">
        <div className="row">
          <Field label="任务类型">
            <select className="in" value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="transcode">视频转码</option>
              <option value="blender">Blender 动画渲染</option>
              <option value="pr">剪辑时间线渲染</option>
              <option value="build">编译打包</option>
            </select>
          </Field>
          {kind === 'transcode' && !adv && (
            <Field label="画质">
              <select className="in" value={quality} onChange={(e) => setQuality(e.target.value)}>
                <option value="fast">快速</option>
                <option value="balanced">均衡（推荐）</option>
                <option value="high">高质量</option>
              </select>
            </Field>
          )}
        </div>

        <Field label="选择文件" hint={kind === 'build' ? '上传源码 zip；解压后的目录就是编译工作目录。' : kind === 'blender' ? '把 .blend 与贴图、缓存打成 zip 上传更稳。' : kind === 'pr' ? '从 Premiere 导出 FCPXML，连同所有素材打包成 zip。' : undefined}>
          <input className="in" type="file" onChange={pickFile} accept={kind === 'build' ? '.zip' : kind === 'blender' ? '.blend,.zip' : kind === 'pr' ? '.zip' : 'video/*,audio/*'} />
        </Field>
        {file && <div className="hint" style={{ marginTop: 6 }}>{file.name}</div>}
      </Card>

      {kind === 'blender' && (
        <Card kicker="Blender" title="场景与输出">
          <div className="row">
            <Field label="场景文件（zip 内可省略）">
              <input className="in" value={f.scene} onChange={(e) => set('scene', e.target.value)} placeholder="scene.blend" />
            </Field>
            <Field label="渲染引擎">
              <select className="in" value={f.engine} onChange={(e) => set('engine', e.target.value)}>
                <option value="">沿用场景设置</option>
                <option value="CYCLES">CYCLES</option>
                <option value="BLENDER_EEVEE_NEXT">BLENDER_EEVEE_NEXT</option>
                <option value="BLENDER_EEVEE">BLENDER_EEVEE</option>
                <option value="BLENDER_WORKBENCH">BLENDER_WORKBENCH</option>
              </select>
            </Field>
          </div>
          <div className="row3">
            <Field label="起始帧">
              <input className="in" inputMode="numeric" value={f.from} onChange={(e) => set('from', e.target.value)} />
            </Field>
            <Field label="结束帧">
              <input className="in" inputMode="numeric" value={f.to} onChange={(e) => set('to', e.target.value)} />
            </Field>
            <Field label="帧率">
              <input className="in" inputMode="decimal" value={f.fps} onChange={(e) => set('fps', e.target.value)} />
            </Field>
          </div>
          <div className="row">
            <Field label="采样数（0=场景）">
              <input className="in" inputMode="numeric" value={f.samples} onChange={(e) => set('samples', e.target.value)} />
            </Field>
            <Field label="分辨率百分比（0=场景）">
              <input className="in" inputMode="numeric" value={f.percent} onChange={(e) => set('percent', e.target.value)} />
            </Field>
          </div>
        </Card>
      )}

      {kind === 'build' && (
        <Card kicker="Build" title="编译打包">
          <Field label="编译命令" hint="在解压后的源码目录中执行；Windows 走 cmd /C，Linux/macOS 走 sh -c。">
            <input
              className="in"
              value={f.command}
              onChange={(e) => set('command', e.target.value)}
              placeholder="powershell -NoProfile -ExecutionPolicy Bypass -File build.ps1"
            />
          </Field>
          <div className="row">
            <Field label="输出目录（相对路径）">
              <input className="in" value={f.output} onChange={(e) => set('output', e.target.value)} placeholder="out" />
            </Field>
            <Field label="超时（秒）">
              <input className="in" inputMode="numeric" value={f.timeout} onChange={(e) => set('timeout', e.target.value)} />
            </Field>
          </div>
          <div className="hint">按实际墙钟分钟结算，预扣 10 分钟；编译失败不结算，差额退还。</div>
        </Card>
      )}

      {adv && (
        <Card kicker="Advanced" title="高级参数">
          {kind === 'transcode' && (
            <>
              <div className="row3">
                <Field label="编码器">
                  <select className="in" value={f.vcodec} onChange={(e) => set('vcodec', e.target.value)}>
                    <option value="h264_nvenc">h264_nvenc</option>
                    <option value="hevc_nvenc">hevc_nvenc</option>
                    <option value="libx264">libx264</option>
                    <option value="libx265">libx265</option>
                  </select>
                </Field>
                <Field label="preset">
                  <select className="in" value={f.preset} onChange={(e) => set('preset', e.target.value)}>
                    {['ultrafast', 'veryfast', 'fast', 'medium', 'slow'].map((p) => (
                      <option key={p}>{p}</option>
                    ))}
                  </select>
                </Field>
                <Field label="QP / CRF">
                  <input className="in" inputMode="numeric" value={f.q} onChange={(e) => set('q', e.target.value)} />
                </Field>
              </div>
              <div className="row">
                <Field label="目标高度（0=原始）">
                  <input className="in" inputMode="numeric" value={f.height} onChange={(e) => set('height', e.target.value)} />
                </Field>
                <Field label="音轨">
                  <select className="in" value={f.audio} onChange={(e) => set('audio', e.target.value)}>
                    <option value="1">保留</option>
                    <option value="0">丢弃</option>
                  </select>
                </Field>
              </div>
              {policy?.local_path && (
                <Field label="或使用服务端本机路径">
                  <input className="in" value={f.path} onChange={(e) => set('path', e.target.value)} placeholder="D:/videos/input.mp4" />
                </Field>
              )}
              <Field label="视频时长（估费用，秒）">
                <input className="in" inputMode="decimal" value={f.duration} onChange={(e) => set('duration', e.target.value)} />
              </Field>
            </>
          )}
          <Field label="分片数">
            <input className="in" inputMode="numeric" value={f.segs} onChange={(e) => set('segs', e.target.value)} />
          </Field>
          <div className="row">
            <Field label="需要独显">
              <select className="in" value={f.need_gpu} onChange={(e) => set('need_gpu', e.target.value)}>
                <option value="0">不限</option>
                <option value="1">必须有 GPU</option>
              </select>
            </Field>
            <Field label="最小显存 MB">
              <input className="in" inputMode="numeric" value={f.min_gpu_mem} onChange={(e) => set('min_gpu_mem', e.target.value)} />
            </Field>
          </div>
          <div className="row">
            <Field label="最少 CPU 核">
              <input className="in" inputMode="numeric" value={f.min_cpus} onChange={(e) => set('min_cpus', e.target.value)} />
            </Field>
            <Field label="最小内存 MB">
              <input className="in" inputMode="numeric" value={f.min_mem} onChange={(e) => set('min_mem', e.target.value)} />
            </Field>
          </div>
          <div className="row">
            <Field label="要求运行时">
              <select className="in" value={f.runtime} onChange={(e) => set('runtime', e.target.value)}>
                <option value="">不限</option>
                <option value="ffmpeg">ffmpeg</option>
                <option value="blender">blender</option>
                <option value="cuda">cuda</option>
                <option value="docker">docker</option>
                <option value="python">python</option>
              </select>
            </Field>
            <Field label="系统架构">
              <select className="in" value={f.arch} onChange={(e) => set('arch', e.target.value)}>
                <option value="">不限</option>
                <option value="amd64">amd64</option>
                <option value="arm64">arm64</option>
              </select>
            </Field>
          </div>
          <Field label="单价上限 点/单位（0=不限）">
            <input className="in" inputMode="decimal" value={f.max_price} onChange={(e) => set('max_price', e.target.value)} />
          </Field>
          {kind === 'transcode' && !policy?.public && (
            <label className="check">
              <input type="checkbox" checked={f.baseline} onChange={(e) => set('baseline', e.target.checked)} />
              同时跑单机基线（用于真实加速比对比）
            </label>
          )}
        </Card>
      )}

      <Card kicker="Quote" title="预计花费">
        {quote ? (
          <>
            <Stats
              items={[
                { v: f2(quote.hold), k: isRender ? '预扣' : '预扣', hl: !!ok },
                { v: quote.eligible, k: '可派发节点' },
                { v: f2(quote.balance), k: '当前余额' },
                ...(quote.cheapest !== undefined && kind === 'transcode' ? [{ v: f2(quote.cheapest), k: '最省' }] : []),
              ]}
            />
            {quote.first_pick && (
              <div className="hint" style={{ marginTop: 8 }}>
                首片预计派给 {quote.first_pick.name}（{f2(quote.first_pick.price)} 点/秒）
              </div>
            )}
            {quote.nodes && quote.nodes.length > 0 && (
              <div className="hint" style={{ marginTop: 6 }}>
                可派发到：{quote.nodes.map((n: any) => `${n.name}（${f2(n.price)}）`).join('、')}
              </div>
            )}
            <div className="tags" style={{ marginTop: 8 }}>
              <Tag tone={ok ? 'ok' : 'warn'}>{ok ? '余额与算力均可用' : quote.affordable ? '暂无匹配节点，任务会排队' : '余额不足'}</Tag>
              {quote.hold_basis && <Tag>{quote.hold_basis} {f2(quote.hold_price)} 点/{quote.unit}</Tag>}
            </div>
          </>
        ) : (
          <div className="hint">{quoteErr || '选择文件后自动估算'}</div>
        )}
        {policy?.min_job_deadline_sec > 0 && (
          <div className="hint" style={{ marginTop: 10 }}>
            任务从提交起最多运行 max({Math.round(policy.min_job_deadline_sec / 60)} 分钟, {f1(policy.job_slow_factor)} × 视频时长)，超时自动失败并退还未使用的预扣。
          </div>
        )}
      </Card>

      <div className="formfoot">
        <Btn block onClick={submit} disabled={busy}>
          {busy ? <span className="spin" /> : null}
          {busy ? '处理中…' : '开始处理'}
        </Btn>
      </div>
      {msg && <div className={'hint' + (msg.startsWith('失败') ? ' warn' : '')}>{msg}</div>}
    </div>
  )
}
