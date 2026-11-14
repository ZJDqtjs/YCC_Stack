import React, { useCallback, useEffect, useState } from 'react'
import { useApp } from '../store.jsx'
import { nav } from '../App.jsx'
import { Card, Tabs, Btn, Tag, Empty, Chip, Field } from '../ui.jsx'
import { OfferCard } from './Market.jsx'
import { api, post, href, f2, CAT_LABEL } from '../api.js'

const UNITS = ['点/次', '点/千 token', '点/视频秒', '点/小时', '点/帧']
const parsePorts = s => {
  const out = []
  for (const raw of String(s || '').split(',').map(x => x.trim()).filter(Boolean)) {
    let proto = 'tcp', t = raw
    if (/\/udp$/i.test(t)) { proto = 'udp'; t = t.replace(/\/udp$/i, '') }
    const m = t.match(/^(\d+):(\d+)$/)
    if (!m) throw new Error('端口映射格式应为 host:guest，例如 2200:22')
    const hp = +m[1], gp = +m[2]
    if (hp < 0 || hp > 65535 || gp < 1 || gp > 65535) throw new Error('端口超出 1~65535 范围')
    out.push({ proto, host_port: hp, guest_port: gp })
  }
  return out
}
export default function Sell({ tab }) {
  const { workers, policy, toast, confirm, prefill, setPrefill } = useApp()
  const [t, setT] = useState(tab || 'publish')
  useEffect(() => { if (tab) setT(tab) }, [tab])
  useEffect(() => {
    if (prefill && prefill.view === 'sell') { setF(x => ({ ...x, title: (prefill.tpl && prefill.tpl.title) || x.title })); setPrefill(null) }
  }, [prefill, setPrefill])
  const [offers, setOffers] = useState([])
  const [cat, setCat] = useState('api')
  const [f, setF] = useState({ title: '', price: '0.8', unit: '点/次', spec: '', body: '', auto: false, node: '', scene: '', frames: '', samples: '' })
  const [cover, setCover] = useState(null)
  const [coverUrl, setCoverUrl] = useState('')
  const [msg, setMsg] = useState('')
  const [vmOffers, setVmOffers] = useState([])
  const [hl, setHl] = useState({ name: '', os: 'windows', arch: 'amd64', max: 2, price: '0.8', slots: 2, bridge: '', uplink: '' })
  const [hlMsg, setHlMsg] = useState('')
  const [v, setV] = useState({ node: '', name: '', image: '', sha: '', fmt: 'qcow2', os: 'ubuntu 24.04', cpus: 2, mem: 2048, disk: 20, host6: '', net: 'slirp', ports: '', prefix: '', gateway: '', price: '1', maxh: 24 })
  const [vmMsg, setVmMsg] = useState('')

  const loadOffers = useCallback(async () => { try { const d = await api('/api/offers'); setOffers(d.offers || []) } catch (e) {} }, [])
  const loadVM = useCallback(async () => { try { const d = await api('/api/vm/offers'); setVmOffers(d.offers || []) } catch (e) {} }, [])
  useEffect(() => { loadOffers(); const iv = setInterval(loadOffers, 5000); return () => clearInterval(iv) }, [loadOffers])
  useEffect(() => { if (t === 'vm') { loadVM(); const iv = setInterval(loadVM, 4000); return () => clearInterval(iv) } }, [t, loadVM])

  const myNodes = workers.filter(w => w.mine)
  const vmNodes = myNodes.filter(w => w.vm && w.online)
  const selNode = workers.find(w => w.id === v.node)
  const mine = offers.filter(o => o.mine)

  const publish = async () => {
    if (!f.title.trim()) { setMsg('标题不能为空'); return }
    const tpl = {}
    if (f.auto) {
      if (f.scene.trim()) tpl.scene = f.scene.trim()
      const m = f.frames.trim().match(/^(\d+)\s*[-~至]\s*(\d+)$/); if (m) { tpl.from = +m[1]; tpl.to = +m[2] }
      if (+f.samples > 0) tpl.samples = +f.samples
      if (!f.node && !Object.keys(tpl).length) { setMsg('开通自动共享需要绑定节点或填写场景任务'); return }
    }
    const fd = new FormData()
    fd.set('category', cat); fd.set('title', f.title.trim()); fd.set('body', f.body.trim()); fd.set('spec', f.spec.trim())
    fd.set('price', f.price || '0'); fd.set('unit', f.unit)
    if (f.auto) { fd.set('auto_share', '1'); if (f.node) fd.set('node_id', f.node); if (Object.keys(tpl).length) fd.set('scene', JSON.stringify(tpl)) }
    if (cover) fd.append('image', cover)
    setMsg('发布中…')
    try {
      const r = await fetch(href('/api/offers'), { method: 'POST', body: fd })
      const txt = await r.text(); let d = null; try { d = JSON.parse(txt) } catch (e) {}
      if (!r.ok) throw (d && d.error) || txt
      setF({ title: '', price: '0.8', unit: '点/次', spec: '', body: '', auto: false, node: '', scene: '', frames: '', samples: '' })
      setCover(null); setCoverUrl(''); setMsg('')
      toast('已发布，服务市场里已经能看到这条服务', 'ok', '发布成功')
      loadOffers(); setT('mine')
    } catch (e) { setMsg('发布失败：' + e) }
  }
  const delOffer = async id => {
    if (!await confirm('删除后这条挂牌会立即从市场消失，确定？', '删除挂牌')) return
    try { await api('/api/offers/' + encodeURIComponent(id), { method: 'DELETE' }); loadOffers(); toast('挂牌已删除', 'ok') } catch (e) { toast(String(e), 'bad') }
  }
  const launch = async () => {
    if (!(+hl.price >= 0.05)) { setHlMsg('转码单价至少 0.05 点/视频秒'); return }
    if (!(+hl.slots >= 1)) { setHlMsg('转码并发槽至少 1 个'); return }
    if (!(+hl.max >= 1 && +hl.max <= 8)) { setHlMsg('云主机并发数需在 1~8 之间'); return }
    setHlMsg('正在生成启动码…')
    try {
      const d = await post('/api/nodes/launch', { name: hl.name.trim() || '云主机节点', price: +hl.price, slots: +hl.slots, os: hl.os, arch: hl.arch, vm: true, vm_max: +hl.max, vm_bridge: hl.bridge.trim(), vm_uplink: hl.uplink.trim() })
      const a = document.createElement('a'); a.href = href(d.script); a.rel = 'noopener'; document.body.appendChild(a); a.click(); a.remove()
      setHlMsg('脚本已下载（' + d.node_id + '，' + Math.max(1, Math.round(d.expires_in_sec / 60)) + ' 分钟内有效）。拷到那台机器运行即可。')
      toast('云主机启动脚本已生成', 'ok')
    } catch (e) { setHlMsg('生成失败：' + e) }
  }
  const publishVM = async () => {
    setVmMsg('正在发布…')
    try {
      const body = { node_id: v.node, name: v.name.trim(), image_url: v.image.trim(), image_sha256: v.sha.trim(), image_format: v.fmt, os: v.os.trim(), cpus: +v.cpus, mem_mb: +v.mem, disk_gb: +v.disk, net_mode: v.net, host_ipv6: v.host6, ports: parsePorts(v.ports), price_hour: +v.price, max_hours: +v.maxh }
      if (v.net === 'routed') { body.ipv6_prefix = v.prefix.trim(); body.ipv6_gateway = v.gateway.trim() }
      if (!body.image_url) throw new Error('请填写镜像 URL')
      await post('/api/vm/offers', body)
      setVmMsg('已发布，买方现在可以下单。'); setV({ ...v, name: '', sha: '' }); loadVM(); toast('配置已发布', 'ok')
    } catch (e) { setVmMsg('发布失败：' + e) }
  }
  const toggleVM = async (id, enabled) => {
    const prev = vmOffers
    setVmOffers(v => v.map(o => o.id === id ? { ...o, enabled } : o))
    try { await api('/api/vm/offers/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) }); toast(enabled ? '已重新上架' : '已下架', 'ok') }
    catch (e) { setVmOffers(prev); toast(String(e), 'bad', '操作失败，已回滚') }
  }
  const delVM = async id => { if (!await confirm('删除后这个配置立即下架；已租出的云主机按订单快照继续运行。确定？', '删除配置')) return; try { await api('/api/vm/offers/' + encodeURIComponent(id), { method: 'DELETE' }); loadVM(); toast('配置已删除', 'ok') } catch (e) { toast(String(e), 'bad') } }

  return (
    <div>
      <div className="phead">
        <div className="phead-txt"><span className="eyebrow">Seller Studio</span><h1>我的发布</h1><p>发布服务挂牌、管理已发布的服务，或把带虚拟机的节点配置成云主机出租。</p></div>
        <div className="phead-actions"><Btn ghost onClick={() => nav('market')}>去市场看看 →</Btn></div>
      </div>
      <Tabs items={[{ key: 'publish', label: '发布服务' }, { key: 'mine', label: '我的服务', count: mine.length }, { key: 'vm', label: '云主机配置' }]} active={t} onChange={setT} />
      {t === 'publish' && (
        <Card kicker="Publish" title="发布你的服务">
          <label>服务类型</label>
          <div className="chips">{Object.keys(CAT_LABEL).map(c => <Chip key={c} cat={c} on={cat === c} onClick={() => setCat(c)}><i className="cdot" />{CAT_LABEL[c]}</Chip>)}</div>
          <div className="row">
            <Field label="标题"><input maxLength={80} value={f.title} onChange={e => setF({ ...f, title: e.target.value })} placeholder="例如 4090 双卡 · Flux 出图" /></Field>
            <Field label="单价（算力点）"><input type="number" min="0" step="0.01" value={f.price} onChange={e => setF({ ...f, price: e.target.value })} /></Field>
          </div>
          <div className="row">
            <Field label="计价单位"><select value={f.unit} onChange={e => setF({ ...f, unit: e.target.value })}>{UNITS.map(u => <option key={u}>{u}</option>)}</select></Field>
            <Field label="配置"><input maxLength={500} value={f.spec} onChange={e => setF({ ...f, spec: e.target.value })} placeholder="例如 双 RTX 4090 · 24G ×2 · ComfyUI" /></Field>
          </div>
          <label className="check"><input type="checkbox" checked={f.auto} onChange={e => setF({ ...f, auto: e.target.checked })} style={{ width: 'auto' }} /><span><b>开通售出自动共享</b><br />买家无需沟通即可直接使用下方节点或场景任务</span></label>
          {f.auto && (
            <div>
              <div className="row" style={{ marginTop: 12 }}>
                <Field label="绑定我的节点" hint={myNodes.length ? `你有 ${myNodes.length} 台节点；离线节点会排队等待上线。` : '你还没有绑定节点，可以只共享场景任务。'}>
                  <select value={f.node} onChange={e => setF({ ...f, node: e.target.value })}><option value="">不绑定节点</option>{myNodes.map(w => <option key={w.id} value={w.id}>{w.name || w.id} · {w.online ? '在线' : '离线'}</option>)}</select>
                </Field>
                <Field label="场景 / 提示词 / 编译命令（可选）"><input maxLength={200} value={f.scene} onChange={e => setF({ ...f, scene: e.target.value })} /></Field>
              </div>
              <div className="row" style={{ marginTop: 12 }}>
                <Field label="帧范围（渲染）"><input maxLength={30} value={f.frames} onChange={e => setF({ ...f, frames: e.target.value })} placeholder="例如 1-120" /></Field>
                <Field label="采样数（渲染）"><input maxLength={8} value={f.samples} onChange={e => setF({ ...f, samples: e.target.value })} placeholder="0=场景设置" /></Field>
              </div>
            </div>
          )}
          <Field label="简介（可选）"><textarea rows={3} maxLength={2000} value={f.body} onChange={e => setF({ ...f, body: e.target.value })} placeholder="说明交付时间、模型、售后和联系方式" /></Field>
          <label>封面配图（可选）</label>
          <label className="cover-pick">
            <input type="file" accept="image/*" onChange={e => { const file = e.target.files[0]; setCover(file || null); setCoverUrl(file ? URL.createObjectURL(file) : '') }} />
            {coverUrl ? <img src={coverUrl} alt="" /> : <div className="cover-ph"><b>点击上传封面</b><span>JPG / PNG / WebP，≤8MB</span></div>}
          </label>
          <div className="formfoot"><Btn onClick={publish}>发布到市场</Btn><div className="hint">{msg}</div></div>
        </Card>
      )}
      {t === 'mine' && (
        <Card kicker="Mine" title="我的发布" side={<Btn ghost tiny onClick={loadOffers}>刷新</Btn>}>
          <div className="mk-grid">{mine.length ? mine.map(o => <OfferCard key={o.id} o={o} manage onOpen={() => nav('market')} onDel={delOffer} />) : <Empty>还没有发布过服务，去「发布服务」发第一条</Empty>}</div>
        </Card>
      )}
      {t === 'vm' && (
        <>
          <Card kicker="Host" title="一键把这台机器接入云主机">
            <div className="row3">
              <Field label="机器名称"><input value={hl.name} onChange={e => setHl({ ...hl, name: e.target.value })} placeholder="例如 宿舍 3060 台式机" /></Field>
              <Field label="那台机器的系统"><select value={hl.os} onChange={e => setHl({ ...hl, os: e.target.value })}><option value="windows">Windows</option><option value="linux">Linux</option><option value="darwin">macOS</option></select></Field>
              <Field label="芯片"><select value={hl.arch} onChange={e => setHl({ ...hl, arch: e.target.value })}><option value="amd64">x86_64</option><option value="arm64">arm64</option></select></Field>
            </div>
            <div className="row3">
              <Field label="最多同时运行几台"><input type="number" min="1" max="8" value={hl.max} onChange={e => setHl({ ...hl, max: e.target.value })} /></Field>
              <Field label="转码单价（点/视频秒）"><input type="number" min="0.05" step="0.05" value={hl.price} onChange={e => setHl({ ...hl, price: e.target.value })} /></Field>
              <Field label="转码并发槽"><input type="number" min="1" value={hl.slots} onChange={e => setHl({ ...hl, slots: e.target.value })} /></Field>
            </div>
            <div className="row">
              <Field label="Linux 网桥（routed 可选）"><input disabled={hl.os !== 'linux'} value={hl.bridge} onChange={e => setHl({ ...hl, bridge: e.target.value })} placeholder="留空用 slirp" /></Field>
              <Field label="上联网卡（NDP 可选）"><input disabled={hl.os !== 'linux'} value={hl.uplink} onChange={e => setHl({ ...hl, uplink: e.target.value })} placeholder="例如 eth0" /></Field>
            </div>
            <div className="formfoot"><Btn onClick={launch}>下载云主机启动脚本</Btn></div>
            <div className="hint">{hlMsg}</div>
          </Card>
          <Card kicker="Sell" title="发布我的虚拟机配置">
            {!vmNodes.length ? <Empty>还没有支持虚拟机的在线节点。先用上面的「一键接入」生成启动脚本；节点上线后这里会出现发布表单。</Empty> : (
              <>
                <div className="row">
                  <Field label="出租节点"><select value={v.node} onChange={e => setV({ ...v, node: e.target.value, host6: '' })}><option value="">选择节点</option>{vmNodes.map(w => <option key={w.id} value={w.id}>{w.name} · {w.cpus} 核</option>)}</select></Field>
                  <Field label="配置名称"><input value={v.name} onChange={e => setV({ ...v, name: e.target.value })} placeholder="例如 2C2G · Ubuntu 24.04" /></Field>
                </div>
                <Field label="系统镜像 URL"><input value={v.image} onChange={e => setV({ ...v, image: e.target.value })} placeholder="https://cloud-images.ubuntu.com/...img" /></Field>
                <div className="row">
                  <Field label="镜像格式"><select value={v.fmt} onChange={e => setV({ ...v, fmt: e.target.value })}><option>qcow2</option><option>raw</option><option>iso</option></select></Field>
                  <Field label="SHA-256（可选）"><input value={v.sha} onChange={e => setV({ ...v, sha: e.target.value })} /></Field>
                </div>
                <div className="row3">
                  <Field label="系统"><input value={v.os} onChange={e => setV({ ...v, os: e.target.value })} /></Field>
                  <Field label="vCPU"><input type="number" min="1" value={v.cpus} onChange={e => setV({ ...v, cpus: e.target.value })} /></Field>
                  <Field label="内存 MiB"><input type="number" min="256" step="256" value={v.mem} onChange={e => setV({ ...v, mem: e.target.value })} /></Field>
                </div>
                <div className="row">
                  <Field label="磁盘 GiB"><input type="number" min="1" value={v.disk} onChange={e => setV({ ...v, disk: e.target.value })} /></Field>
                  <Field label="卖方公网 IPv6"><select value={v.host6} onChange={e => setV({ ...v, host6: e.target.value })}><option value="">选择 IPv6</option>{((selNode && selNode.ipv6) || []).map(ip => <option key={ip}>{ip}</option>)}</select></Field>
                </div>
                <div className="row">
                  <Field label="网络模式"><select value={v.net} onChange={e => setV({ ...v, net: e.target.value })}><option value="slirp">slirp（推荐）</option><option value="routed">routed</option></select></Field>
                  <Field label="端口映射 host:guest"><input disabled={v.net === 'routed'} value={v.ports} onChange={e => setV({ ...v, ports: e.target.value })} placeholder="2200:22, 8080:80" /></Field>
                </div>
                {v.net === 'routed' && (
                  <div className="row">
                    <Field label="IPv6 前缀"><input value={v.prefix} onChange={e => setV({ ...v, prefix: e.target.value })} placeholder="2001:db8:42::/64" /></Field>
                    <Field label="IPv6 网关"><input value={v.gateway} onChange={e => setV({ ...v, gateway: e.target.value })} /></Field>
                  </div>
                )}
                <div className="row">
                  <Field label="价格（点/小时）"><input type="number" min="0.01" step="0.5" value={v.price} onChange={e => setV({ ...v, price: e.target.value })} /></Field>
                  <Field label="最长租期（小时）"><input type="number" min="1" max="720" value={v.maxh} onChange={e => setV({ ...v, maxh: e.target.value })} /></Field>
                </div>
                <div className="formfoot"><Btn onClick={publishVM} disabled={!v.node}>发布配置</Btn><div className="hint">{vmMsg}</div></div>
              </>
            )}
          </Card>
          <Card kicker="My Offers" title="我发布的配置">
            {vmOffers.filter(o => o.mine).length ? vmOffers.filter(o => o.mine).map(o => (
              <div className="job" key={o.id} style={{ marginBottom: 10 }}>
                <h3>{o.name} {o.enabled ? <Tag tone="on">已上架</Tag> : <Tag tone="off">已下架</Tag>} <Tag>{o.cpus}C / {Math.round(o.mem_mb / 1024 * 10) / 10}G / {o.disk_gb}G</Tag> <Tag>{f2(o.price_hour)} 点/小时</Tag>
                  <Btn ghost tiny onClick={() => toggleVM(o.id, !o.enabled)}>{o.enabled ? '下架' : '重新上架'}</Btn>
                  <Btn ghost tiny danger onClick={() => delVM(o.id)}>删除</Btn></h3>
                <div className="note">{o.node_name || o.node_id} · {o.os} · {o.available ? '节点在线' : '节点离线'}</div>
              </div>
            )) : <Empty>还没有发布配置</Empty>}
          </Card>
        </>
      )}
    </div>
  )
}