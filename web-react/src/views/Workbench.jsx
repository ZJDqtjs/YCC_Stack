import React, { useEffect, useRef, useState, useCallback } from 'react'
import { useApp } from '../store.jsx'
import { nav } from '../App.jsx'
import { Card, Kpi, Btn, Tag, Empty } from '../ui.jsx'
import { api, f1, f2, wbSize } from '../api.js'
import { packFolder, extOk } from '../zip.js'

const KIND_NAME = { transcode: '视频转码', blender: 'Blender 渲染', pr: '时间线渲染', build: '编译打包' }
const DROP_HINT = { transcode: '视频 / 音频文件，自动读取时长', blender: '.blend 场景，或含贴图 / 缓存的 zip', pr: 'FCPXML 工程 + 全部素材打包的 zip', build: '源码 zip，解压目录即工作目录' }
const QUALITY = { fast: { vcodec: 'libx264', preset: 'ultrafast', q: '28' }, balanced: { vcodec: 'libx264', preset: 'fast', q: '26' }, high: { vcodec: 'libx264', preset: 'slow', q: '20' } }
const CMD_CHIPS = ['powershell -NoProfile -ExecutionPolicy Bypass -File build.ps1', 'npm install && npm run build', 'go build -o out/app ./...', 'cmake -B build && cmake --build build --config Release']

export default function Workbench({ svc }) {
  const { workers, policy, advanced, target, setTarget, toast, prefill, setPrefill } = useApp()
  const [kind, setKind] = useState(svc === 'build' ? 'build' : 'transcode')
  const [file, setFile] = useState(null)
  const [folderInfo, setFolderInfo] = useState('')
  const [dur, setDur] = useState(0)
  const [quality, setQuality] = useState('balanced')
  const [version, setVersion] = useState(0)
  const [est, setEst] = useState(null)
  const [estErr, setEstErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const formRef = useRef(null)
  const fileRef = useRef(null)
  const folderRef = useRef(null)

  useEffect(() => { setKind(svc === 'build' ? 'build' : (kind === 'build' ? 'transcode' : kind)) }, [svc])

  // 自动共享直用：把卖家场景模板预填进表单
  useEffect(() => {
    if (prefill && (prefill.view === 'render' || prefill.view === 'build') && prefill.view === svc) {
      const tpl = prefill.tpl || {}
      if (prefill.kind) setKind(prefill.kind)
      else if (svc === 'render' && (tpl.scene || tpl.from || tpl.to || tpl.samples)) setKind('blender')
      setTimeout(() => {
        const names = svc === 'build' ? ['command', 'output', 'timeout'] : ['scene', 'engine', 'samples', 'from', 'to', 'fps', 'percent']
        names.forEach(n => { if (tpl[n] !== undefined && tpl[n] !== null && tpl[n] !== '') { const el = formRef.current && formRef.current.querySelector('[name=' + n + ']'); if (el) el.value = tpl[n] } })
        if (svc === 'build' && !tpl.command && tpl.scene) { const el = formRef.current && formRef.current.querySelector('[name=command]'); if (el) el.value = tpl.scene }
        bump()
      }, 0)
      setPrefill(null)
    }
  }, [prefill, svc, setPrefill])

  const g = useCallback(n => {
    const el = formRef.current && formRef.current.querySelector('[name=' + n + ']')
    return el ? el.value : ''
  }, [])
  const bump = () => setVersion(v => v + 1)

  // 画质预设写入字段
  useEffect(() => {
    if (advanced || kind !== 'transcode') return
    const m = QUALITY[quality] || QUALITY.balanced
    for (const k in m) { const el = formRef.current && formRef.current.querySelector('[name=' + k + ']'); if (el) el.value = m[k] }
    bump()
  }, [quality, advanced, kind])

  const reqFromForm = useCallback(() => {
    const r = {}
    if (g('need_gpu') === '1') r.need_gpu = true
    if (+g('min_gpu_mem') > 0) r.min_gpu_mem_mb = +g('min_gpu_mem')
    if (+g('min_cpus') > 0) r.min_cpus = +g('min_cpus')
    if (+g('min_mem') > 0) r.min_mem_mb = +g('min_mem')
    if (+g('max_price') > 0) r.max_price = +g('max_price')
    if (g('runtime')) r.runtime = g('runtime')
    if (g('arch')) r.arch = g('arch')
    if (target) r.node_id = target
    return r
  }, [g, target])

  // 估价（debounce）
  useEffect(() => {
    const t = setTimeout(async () => {
      if (kind === 'build') { setEst({ build: true }); setEstErr(''); return }
      if (kind !== 'transcode') {
        let units = 1
        if (kind === 'blender') { units = (+g('to') || 0) - (+g('from') || 0) + 1; if (!(units > 0)) { setEst(null); setEstErr('填写起止帧后按帧数估算'); return } }
        try {
          const e = await api('/api/estimate?duration=' + units + '&kind=' + kind + '&vcodec=libx264&req=' + encodeURIComponent(JSON.stringify(reqFromForm())))
          setEst(e); setEstErr('')
        } catch (err) { setEst(null); setEstErr('估算失败：' + err) }
        return
      }
      const d = +g('duration') || dur || 0
      if (!(d > 0)) { setEst(null); setEstErr('填视频时长或选择文件后自动估算'); return }
      try {
        const e = await api('/api/estimate?duration=' + d + '&vcodec=' + g('vcodec') + '&req=' + encodeURIComponent(JSON.stringify(reqFromForm())))
        setEst(e); setEstErr('')
      } catch (err) { setEst(null); setEstErr('估算失败：' + err) }
    }, 250)
    return () => clearTimeout(t)
  }, [kind, quality, file, dur, version, advanced, target, reqFromForm, g])

  const onFileChange = e => {
    const f = e.target.files[0] || null
    setFile(f); setFolderInfo(''); bump()
    if (f && kind === 'transcode') {
      const v = document.createElement('video'); v.preload = 'metadata'
      v.onloadedmetadata = () => { if (isFinite(v.duration) && v.duration > 0) { setDur(v.duration); const el = formRef.current.querySelector('[name=duration]'); if (el) el.value = Math.round(v.duration); bump() } }
      v.src = URL.createObjectURL(f)
    }
  }
  const onDrop = e => {
    e.preventDefault()
    const files = e.dataTransfer && e.dataTransfer.files
    if (!files || !files.length) return
    if (!extOk(files[0].name, kind)) { setMsg('文件类型不匹配：' + (DROP_HINT[kind] || '')); return }
    fileRef.current.files = files
    onFileChange({ target: { files } })
  }
  const onFolder = async e => {
    if (!e.target.files || !e.target.files.length) return
    setMsg('正在打包文件夹…')
    try {
      const r = await packFolder(e.target.files)
      const f = new File([r.blob], r.root + '.zip', { type: 'application/zip' })
      const dt = new DataTransfer(); dt.items.add(f)
      fileRef.current.files = dt.files
      setFile(f); setFolderInfo('来自文件夹「' + r.root + '」· ' + r.count + ' 个文件 · ' + wbSize(r.total)); setMsg(''); bump()
    } catch (err) { setMsg('打包失败：' + (err.message || err)) }
    finally { e.target.value = '' }
  }
  const clearFile = () => { fileRef.current.value = ''; setFile(null); setFolderInfo(''); if (kind === 'transcode') { setDur(0); const el = formRef.current.querySelector('[name=duration]'); if (el) el.value = '0' } bump() }

  const submit = async e => {
    e.preventDefault()
    const fd = new FormData(formRef.current)
    if (!fd.get('file') || !fd.get('file').size) fd.delete('file')
    fd.set('req', JSON.stringify(reqFromForm()))
    ;['need_gpu', 'min_gpu_mem', 'min_cpus', 'min_mem', 'max_price', 'runtime', 'arch', 'duration'].forEach(n => fd.delete(n))
    let url = '/api/jobs'
    if (kind === 'transcode') {
      if (!fd.get('baseline')) fd.set('baseline', '0')
      ;['jobkind', 'scene', 'engine', 'samples', 'from', 'to', 'percent', 'fps'].forEach(n => fd.delete(n))
    } else if (kind === 'build') {
      url = '/api/jobs/build'
      ;['jobkind', 'scene', 'engine', 'samples', 'from', 'to', 'percent', 'fps', 'vcodec', 'preset', 'q', 'height', 'audio', 'baseline', 'path', 'duration'].forEach(n => fd.delete(n))
      fd.set('command', String(fd.get('command') || '').trim())
      fd.set('output', String(fd.get('output') || '').trim())
      fd.set('timeout', String(fd.get('timeout') || '1800').trim())
      if (!fd.get('file')) { setMsg('请上传源码 zip'); return }
      if (!fd.get('command')) { setMsg('请填写编译命令'); return }
    } else {
      url = '/api/jobs/render'
      fd.set('kind', kind)
      ;['baseline', 'path', 'vcodec', 'preset', 'q', 'height', 'audio', 'jobkind'].forEach(n => fd.delete(n))
      if (kind !== 'blender') ['scene', 'engine', 'samples', 'from', 'to', 'percent', 'fps'].forEach(n => fd.delete(n))
    }
    setBusy(true); setMsg('上传并登记任务中…')
    try {
      const d = await api(url, { method: 'POST', body: fd })
      setTarget(null)
      toast('任务 ' + d.id + ' 已入队，预扣 ' + f2(d.hold) + ' 点', 'ok', '提交成功')
      nav('jobs')
    } catch (err) { setMsg('失败：' + err); toast(String(err), 'bad', '提交失败') }
    finally { setBusy(false) }
  }

  const on = workers.filter(w => w.online)
  const free = on.reduce((a, w) => a + Math.max(0, (w.slots || 0) - (w.active || 0)), 0)
  const prices = on.map(w => Number(w.price) || 0).filter(p => p > 0)
  const min = prices.length ? Math.min(...prices) : null
  const showAdv = advanced || kind !== 'transcode'
  const hasFile = !!file
  const cfg = kind === 'build' ? !!g('command').trim() : true
  const ready = est && (est.build || (est.affordable && est.eligible > 0))
  const targetW = workers.find(w => w.id === target)

  return (
    <div>
      <div className="phead">
        <div className="phead-txt">
          <span className="eyebrow">{svc === 'build' ? 'Pipeline · Orchestration' : 'Render Studio'}</span>
          <h1>{svc === 'build' ? '任务编排' : '渲染工作台'}</h1>
          <p>{svc === 'build' ? '源码 zip 上传后由算力节点执行编译 / 打包命令，产物按输出目录回传；按墙钟分钟结算，失败不结算。' : 'Blender 帧渲染、剪辑时间线与视频转码共用一套提交流程；参数在本页填写，提交后按实测用量结算。'}</p>
        </div>
        <div className="phead-actions"><Btn ghost onClick={() => nav('jobs')}>我的任务 →</Btn></div>
      </div>
      {svc === 'build' && (
        <div className="wb-pipe">
          <div className="wb-pipe-step"><b>① 上传源码</b><span>zip 打包上传，解压后的目录就是工作目录</span></div>
          <i className="wb-pipe-line" />
          <div className="wb-pipe-step"><b>② 节点编译</b><span>开启 --allow-build 的节点执行命令，按墙钟分钟计费</span></div>
          <i className="wb-pipe-line" />
          <div className="wb-pipe-step"><b>③ 产物回传</b><span>输出目录打包 result.zip，校验后在我的任务下载</span></div>
        </div>
      )}
      <div className="kpis">
        <Kpi label="在线节点" value={on.length} icon="◉" />
        <Kpi label="空闲槽位" value={free} icon="▦" />
        <Kpi label="最低单价" value={min === null ? '—' : f2(min) + ' 点/秒'} icon="◈" />
        <Kpi action={<Btn ghost tiny onClick={() => nav('market')}>去市场看卖家 →</Btn>} />
      </div>
      {target && (
        <div className="targetchip">仅派发到 <b>{targetW ? (targetW.name || targetW.id) : target}</b>
          <Btn ghost tiny onClick={() => { setTarget(null); bump() }}>取消指定</Btn></div>
      )}
      <div className="grid" id="wb" data-svc={svc}>
        <Card kicker="New Task" title={svc === 'build' ? '提交编排任务' : '提交渲染任务'}>
          <div className="wb-steps">
            <span className={'wb-step ' + (hasFile ? 'done' : 'cur')}><b>1</b>上传素材</span><i className="wb-step-line" />
            <span className={'wb-step ' + (hasFile ? (cfg ? 'done' : 'cur') : '')}><b>2</b>配置参数</span><i className="wb-step-line" />
            <span className={'wb-step ' + (hasFile && cfg && ready ? 'cur' : '')}><b>3</b>提交结算</span>
          </div>
          <form ref={formRef} onSubmit={submit} onInput={bump} onChange={bump}>
            <select name="jobkind" className="sr" value={kind} onChange={e => setKind(e.target.value)} aria-label="任务类型">
              <option value="transcode">视频转码</option><option value="blender">Blender 动画渲染</option>
              <option value="pr">剪辑时间线渲染</option><option value="build">编译打包</option>
            </select>
            {svc !== 'build' && (
              <div className="wb-kinds">
                {[['transcode', '视频转码', '切片并行加速，哈希校验一致性', '按视频秒结算', 'video'],
                  ['blender', 'Blender 渲染', '帧级拆分到多节点，Cycles / Eevee', '按帧结算', 'render'],
                  ['pr', '时间线渲染', 'Premiere FCPXML 工程整线输出', '按时间线秒结算', 'jobs']].map(([k, t, d, tag, icon]) => (
                  <button type="button" key={k} className={'wb-kind' + (kind === k ? ' on' : '')} onClick={() => setKind(k)}>
                    <b>{t}</b><span>{d}</span><span className="wb-kind-tag">{tag}</span>
                  </button>
                ))}
              </div>
            )}
            {!advanced && kind === 'transcode' && (
              <div><label>画质</label><select value={quality} onChange={e => setQuality(e.target.value)}>
                <option value="fast">快速</option><option value="balanced">均衡（推荐）</option><option value="high">高质量</option></select></div>
            )}
            <div className="wb-sec"><i>1</i>上传素材<small>支持拖拽上传</small></div>
            {kind === 'build' && (
              <div className="wb-pick">
                <Btn sm type="button" onClick={() => folderRef.current.click()}>选择项目文件夹</Btn>
                <span className="wb-pick-or">自动打包为 zip，排除 node_modules / .git 等目录</span>
              </div>
            )}
            <input ref={folderRef} type="file" className="sr" webkitdirectory="true" directory="" multiple onChange={onFolder} />
            <div className={'wb-drop' + (hasFile ? ' has-file' : '')} role="button" tabIndex={0}
              onClick={() => fileRef.current.click()}
              onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileRef.current.click() } }}
              onDragOver={e => e.preventDefault()} onDrop={onDrop}>
              <input ref={fileRef} type="file" name="file" className="sr" onChange={onFileChange}
                accept={kind === 'build' ? '.zip' : kind === 'blender' ? '.blend,.zip' : kind === 'pr' ? '.zip' : 'video/*,audio/*'} />
              {!hasFile ? (
                <div className="wb-drop-ph"><b>拖拽文件到此处，或点击选择</b><span>{DROP_HINT[kind]}</span></div>
              ) : (
                <div className="wb-file">
                  <span className="wb-file-name">{file.name}</span>
                  <span className="wb-file-size">{wbSize(file.size)}</span>
                  <button type="button" className="wb-file-x" onClick={e => { e.stopPropagation(); clearFile() }} aria-label="移除文件">✕</button>
                </div>
              )}
            </div>
            {folderInfo && <div className="hint">{folderInfo}</div>}

            {showAdv && (
              <div>
                {kind === 'transcode' && policy && policy.local_path && (
                  <div><label>…或使用服务端本机路径</label><input name="path" placeholder="D:/videos/input.mp4" /></div>
                )}
                {kind !== 'build' && (
                  <div className="row"><div><label>切分片数</label><input name="segs" defaultValue="4" inputMode="numeric" /></div><div /></div>
                )}
                {kind === 'blender' && (
                  <fieldset className="fieldset"><legend>Blender 场景与输出</legend>
                    <div className="row3">
                      <div><label>场景文件</label><input name="scene" placeholder="scene.blend" /></div>
                      <div><label>渲染引擎</label><select name="engine"><option value="">沿用场景设置</option><option>CYCLES</option><option>BLENDER_EEVEE_NEXT</option><option>BLENDER_EEVEE</option><option>BLENDER_WORKBENCH</option></select></div>
                      <div><label>采样数（0=场景）</label><input name="samples" defaultValue="0" inputMode="numeric" /></div>
                    </div>
                    <div className="row3">
                      <div><label>起始帧</label><input name="from" defaultValue="1" inputMode="numeric" /></div>
                      <div><label>结束帧</label><input name="to" defaultValue="120" inputMode="numeric" /></div>
                      <div><label>输出帧率</label><input name="fps" defaultValue="24" inputMode="decimal" /></div>
                    </div>
                    <div className="row"><div><label>分辨率百分比（0=场景）</label><input name="percent" defaultValue="0" inputMode="numeric" /></div><div className="hint">把 .blend 和贴图、缓存打成 zip 上传更稳。</div></div>
                  </fieldset>
                )}
                {kind === 'pr' && <div className="hint">上传 zip：从 Premiere 导出 Final Cut Pro XML（或 FCPXML），连同所有素材一起打包。</div>}
                {kind === 'build' && (
                  <div>
                    <div className="wb-sec"><i>2</i>编译配置<small>zip → result.zip</small></div>
                    <label>编译命令</label>
                    <div className="wb-cmd"><span className="wb-cmd-ps">$</span><input name="command" placeholder="powershell -NoProfile -ExecutionPolicy Bypass -File build.ps1" /></div>
                    <div className="wb-cmd-chips">
                      {CMD_CHIPS.map(c => <button type="button" key={c} className="wb-cmd-chip" onClick={() => { const el = formRef.current.querySelector('[name=command]'); el.value = c; bump() }}>{c.split(' ')[0]}</button>)}
                    </div>
                    <div className="row">
                      <div><label>输出目录（相对路径）</label><input name="output" defaultValue="out" /></div>
                      <div><label>超时（秒）</label><input name="timeout" defaultValue="1800" inputMode="numeric" /></div>
                    </div>
                    <div className="hint">按实际墙钟分钟结算，预扣 10 分钟；编译失败不结算。</div>
                  </div>
                )}
                {kind === 'transcode' && (
                  <fieldset className="fieldset"><legend>编码参数</legend>
                    <div className="row3">
                      <div><label>目标编码器</label><select name="vcodec"><option value="h264_nvenc">h264_nvenc (GPU)</option><option value="hevc_nvenc">hevc_nvenc (GPU)</option><option value="libx264">libx264 (CPU)</option><option value="libx265">libx265 (CPU)</option></select></div>
                      <div><label>preset</label><select name="preset"><option>ultrafast</option><option>veryfast</option><option>fast</option><option>medium</option><option>slow</option></select></div>
                      <div><label>质量 QP/CRF</label><input name="q" defaultValue="26" inputMode="numeric" /></div>
                    </div>
                    <div className="row">
                      <div><label>目标高度 (0=原始)</label><input name="height" defaultValue="0" inputMode="numeric" /></div>
                      <div><label>音轨</label><select name="audio"><option value="1">保留</option><option value="0">丢弃</option></select></div>
                    </div>
                  </fieldset>
                )}
                <details className="wb-adv" open={advanced}>
                  <summary>高级设置 · 硬件约束与调度偏好</summary>
                  <div className="wb-adv-body">
                    <fieldset className="fieldset"><legend>硬件要求（可选）</legend>
                      <div className="row">
                        <div><label>需要独显</label><select name="need_gpu"><option value="0">不限</option><option value="1">必须有 GPU</option></select></div>
                        <div><label>最小显存 MB</label><input name="min_gpu_mem" defaultValue="0" inputMode="numeric" /></div>
                      </div>
                      <div className="row">
                        <div><label>最少 CPU 核</label><input name="min_cpus" defaultValue="0" inputMode="numeric" /></div>
                        <div><label>最小内存 MB</label><input name="min_mem" defaultValue="0" inputMode="numeric" /></div>
                      </div>
                      <div className="row">
                        <div><label>要求运行时</label><select name="runtime"><option value="">不限</option><option>ffmpeg</option><option>blender</option><option>cuda</option><option>docker</option><option>python</option></select></div>
                        <div><label>限定系统架构</label><select name="arch"><option value="">不限</option><option>amd64</option><option>arm64</option></select></div>
                      </div>
                      <div className="row">
                        <div><label>单价上限 点/单位</label><input name="max_price" defaultValue="0" inputMode="decimal" /></div>
                        {kind === 'transcode' && <div><label>视频时长（估费用用，秒）</label><input name="duration" defaultValue="0" inputMode="decimal" /></div>}
                      </div>
                    </fieldset>
                    {kind === 'transcode' && !(policy && policy.public) && (
                      <label className="check"><input type="checkbox" name="baseline" value="1" defaultChecked style={{ width: 'auto' }} />同时跑单机基线（用于真实加速比对比）</label>
                    )}
                  </div>
                </details>
              </div>
            )}
          </form>
        </Card>
        <div className="sticky">
          <Card kicker="Summary" title="费用摘要">
            <div className="wb-sumrows">
              <div className="wb-sumrow"><span>任务类型</span><b>{KIND_NAME[kind]}</b></div>
              <div className="wb-sumrow"><span>素材</span><b>{file ? file.name : '未选择'}</b></div>
            </div>
            <div className="quote">
              {estErr && <div className="q-note">{estErr}</div>}
              {est && est.build && <div className="q-note">编译任务按实际墙钟分钟结算，预扣 <b>10</b> 分钟；编译失败不结算，差额自动退还。</div>}
              {est && !est.build && (
                <>
                  <div className="q-line">可用节点 <b>{est.eligible}</b> 台</div>
                  <div className={'q-total' + (est.affordable && est.eligible > 0 ? '' : ' no')}>
                    <span>预扣</span><span className="big">{f2(est.hold)}</span><span className="q-unit">点</span>
                    <span className="q-basis">{est.hold_basis} {f2(est.hold_price)} 点/{est.unit}</span>
                  </div>
                  <div className="q-line">余额 {f2(est.balance)} 点 {est.affordable ? '· 足够' : '· 不足，请到钱包充值'}</div>
                  {est.eligible > 0
                    ? <div className="q-nodes">{est.nodes.map(n => <Tag key={n.name} tone={n.mine ? 'mine' : ''}>{n.name} · {f2(n.price)} 点/{est.unit}</Tag>)}</div>
                    : <div className="q-warn">没有在线节点满足要求，任务会排队等待。</div>}
                </>
              )}
              {!est && !estErr && <div className="q-note">选择文件后自动估算</div>}
            </div>
            <Btn block className="wb-submit" type="button" disabled={busy}
              onClick={() => formRef.current.requestSubmit()}>
              {kind === 'build' ? '提交编排任务' : kind === 'transcode' ? '开始处理' : '提交渲染任务'}
            </Btn>
            <div className="hint">{msg}</div>
          </Card>
          {!advanced && (
            <Card kicker="Tips" title="想要更多控制？">
              <div className="tip" style={{ margin: 0 }}>到「设置」打开高级选项，可以自选编码器、硬件要求、分片数与服务端本机路径。</div>
            </Card>
          )}
        </div>
      </div>
    </div>
  )
}