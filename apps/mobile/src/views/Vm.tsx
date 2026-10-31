import React, { useEffect, useState } from 'react'
import { useApp } from '../ctx'
import { Bar, Btn, Card, Empty, Field, Loader, Sheet, Stats, Tag } from '../ui'
import { hms, f2, memGB, portsLabel, ts } from '../../../shared/index.js'
import { VM_ST, toneOf } from '../../../shared/constants.js'
import { openExternal } from '../api'

const VM_DEFAULTS = {
  vhlname: '',
  vhlos: 'windows',
  vhlarch: 'amd64',
  vhlmax: '2',
  vhlprice: '0.8',
  vhlslots: '2',
  vhlbridge: '',
  vhlup: '',
}

const PUB_DEFAULTS = {
  node_id: '',
  name: '',
  image_url: '',
  image_format: 'qcow2',
  sha: '',
  os: 'ubuntu 24.04',
  cpus: '2',
  mem: '2048',
  disk: '20',
  host_ipv6: '',
  net_mode: 'slirp',
  ports: '',
  price: '1',
  maxh: '24',
  prefix: '',
  gateway: '',
}

export default function Vm() {
  const { api, workers, showToast } = useApp()
  const [offers, setOffers] = useState<any[] | null>(null)
  const [instances, setInstances] = useState<any[] | null>(null)
  const [renting, setRenting] = useState<any>(null)
  const [order, setOrder] = useState({ name: '', hours: '1', key: '' })
  const [creds, setCreds] = useState('')
  const [showPublish, setShowPublish] = useState(false)
  const [pub, setPub] = useState<any>({ ...PUB_DEFAULTS })
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [launch, setLaunch] = useState<any>({ ...VM_DEFAULTS })
  const [launchMsg, setLaunchMsg] = useState('')
  const setP = (k: string, v: any) => setPub((p: any) => ({ ...p, [k]: v }))
  const setL = (k: string, v: any) => setLaunch((p: any) => ({ ...p, [k]: v }))

  const load = async () => {
    try {
      const [o, i] = await Promise.all([api.vmOffers(), api.vmInstances()])
      setOffers(o.offers || [])
      setInstances(i.instances || [])
    } catch (e: any) {
      showToast('读取云主机失败：' + (e.message || e), 'bad')
      setOffers([])
      setInstances([])
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const ownedNodes = (workers || []).filter((w: any) => w.mine && w.vm)
  const onlineOwned = ownedNodes.filter((w: any) => w.online)

  useEffect(() => {
    if (!pub.node_id && onlineOwned.length) {
      setPub((p: any) => ({ ...p, node_id: onlineOwned[0].id, host_ipv6: (onlineOwned[0].ipv6 || [])[0] || '' }))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workers])

  const node = onlineOwned.find((w: any) => w.id === pub.node_id)
  const nodeIPv6 = (node && node.ipv6) || []

  const startRent = (o: any) => {
    setRenting(o)
    setOrder({ name: o.name, hours: '1', key: '' })
    setCreds('')
  }

  const submitRent = async () => {
    if (!renting) return
    setBusy(true)
    setMsg('正在预扣并下发启动指令…')
    try {
      const r = await api.vmOrder({ offer_id: renting.id, name: order.name.trim(), hours: Number(order.hours) || 1, ssh_public_key: order.key.trim() })
      const lines = [
        `主机：${r.instance.name}（${r.instance.id}）`,
        `状态：${VM_ST[r.instance.status] || r.instance.status} · 预扣 ${f2(r.instance.hold)} 点 · 最长 ${renting.max_hours} 小时`,
        `SSH 用户：${r.ssh_user}`,
        r.ssh_private_key ? `SSH 私钥（只显示这一次）：\n${r.ssh_private_key}` : 'SSH：使用你填写的公钥登录',
        `root 口令：${r.root_password}`,
        `VNC 口令：${r.vnc_password}`,
        '启动完成后在「我的云主机」里查看 IPv6 / SSH 地址。',
      ]
      setCreds((r.warning ? r.warning + '\n\n' : '') + lines.join('\n\n'))
      setMsg('下单成功，等待卖方机器启动虚拟机。')
      setRenting(null)
      load()
    } catch (e: any) {
      setMsg('失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const submitPublish = async () => {
    const body: any = {
      node_id: pub.node_id,
      name: pub.name.trim(),
      image_url: pub.image_url.trim(),
      image_sha256: pub.sha.trim(),
      image_format: pub.image_format,
      os: pub.os.trim(),
      cpus: Number(pub.cpus),
      mem_mb: Number(pub.mem),
      disk_gb: Number(pub.disk),
      net_mode: pub.net_mode,
      host_ipv6: pub.host_ipv6,
      ports: String(pub.ports || '')
        .split(',')
        .map((x) => x.trim())
        .filter(Boolean)
        .map((raw) => {
          const proto = /\/udp$/i.test(raw) ? 'udp' : 'tcp'
          const t = raw.replace(/\/udp$/i, '')
          const m = t.match(/^(\d+):(\d+)$/)
          if (!m) throw new Error('端口映射格式应为 host:guest，例如 2200:22')
          return { proto, host_port: Number(m[1]), guest_port: Number(m[2]) }
        }),
      price_hour: Number(pub.price),
      max_hours: Number(pub.maxh),
    }
    if (pub.net_mode === 'routed') {
      body.ipv6_prefix = pub.prefix.trim()
      body.ipv6_gateway = pub.gateway.trim()
    }
    if (!body.image_url) return showToast('请填写镜像 URL', 'bad')
    setBusy(true)
    setMsg('正在发布…')
    try {
      await api.vmPublish(body)
      setMsg('已发布，买方现在可以下单。')
      setPub((p: any) => ({ ...p, name: '', sha: '' }))
      await load()
    } catch (e: any) {
      setMsg('发布失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const launchVMHost = async () => {
    const body = {
      name: launch.vhlname.trim() || '云主机节点',
      price: Number(launch.vhlprice),
      slots: Number(launch.vhlslots),
      os: launch.vhlos,
      arch: launch.vhlarch,
      vm: true,
      vm_max: Number(launch.vhlmax),
      vm_bridge: launch.vhlos === 'linux' ? launch.vhlbridge.trim() : '',
      vm_uplink: launch.vhlos === 'linux' ? launch.vhlup.trim() : '',
    }
    if (!(body.price >= 0.05)) return showToast('转码单价至少 0.05 点/视频秒', 'bad')
    if (!(body.slots >= 1)) return showToast('并发槽至少 1 个', 'bad')
    if (!(body.vm_max >= 1 && body.vm_max <= 8)) return showToast('云主机并发数需在 1~8 之间', 'bad')
    setBusy(true)
    setLaunchMsg('正在生成启动码…')
    try {
      const d = await api.launchNode(body)
      openExternal(api.launchScriptUrl(d.script))
      setLaunchMsg(`脚本已开始下载（${d.node_id}）。在那台机器上运行；脚本会检查 QEMU 并启动 Agent 绑定到你的账户。`)
    } catch (e: any) {
      setLaunchMsg('生成失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const myOffers = (offers || []).filter((o: any) => o.mine)
  const others = (offers || []).filter((o: any) => !o.mine && o.enabled)

  const offerCard = (o: any) => {
    const remain = o.remaining_sec > 0 ? (o.status === 'provisioning' ? hms(o.remaining_sec) : hms(o.remaining_sec)) : '—'
    const ep = o.endpoints || {}
    const actions: any[] = []
    if (o.mine && ['queued', 'provisioning', 'running', 'unreachable'].includes(o.status)) actions.push(['续费', () => api.vmExtend(o.id, o.price_hour).then(load).catch((e: any) => showToast('续费失败：' + e.message, 'bad'))])
    if ((o.mine || o.seller_mine) && !['stopped', 'failed'].includes(o.status)) actions.push([o.mine ? '停机' : '强制停机', () => api.vmStop(o.id).then(load).catch((e: any) => showToast('停机失败：' + e.message, 'bad'))])
    return (
      <div key={o.id} style={{ paddingTop: 12 }}>
        <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          {o.name}
          <Tag tone={toneOf(o.status)}>{VM_ST[o.status] || o.status}</Tag>
          {o.seller_mine && <Tag tone="link">卖方视角</Tag>}
        </h3>
        <div className="tags">
          <Tag>{o.cpus}C / {memGB(o.mem_mb)} / {o.disk_gb}G</Tag>
          <Tag>{o.net_mode}</Tag>
          <Tag>{f2(o.price_hour)} 点/小时</Tag>
        </div>
        <div className="hint" style={{ marginTop: 6 }}>
          {o.node_name || o.node_id} · {o.os} · 预扣 {f2(o.hold)} · 已用 {f2(o.spent)}
        </div>
        {(o.status === 'provisioning' || o.status === 'queued') && (
          <div className="prog">
            <Bar pct={o.progress} />
            <span className="sub">{Math.round(o.progress || 0)}%</span>
          </div>
        )}
        <div className="hint" style={{ marginTop: 6 }}>
          SSH {ep.ssh ? <code>{ep.ssh_user || o.ssh_user}@{ep.ssh}</code> : '—'}
          {ep.ipv6 ? ' · IPv6 ' : ''}
          {ep.ipv6 && <code>{ep.ipv6}</code>}
        </div>
        {ep.vnc && <div className="hint">控制台 <code>{ep.vnc}</code></div>}
        {(ep.forwards || []).length > 0 && (
          <div className="hint">
            端口{' '}
            {(ep.forwards || []).map((fwd: any, i: number) => (
              <span key={i}>
                <code>{fwd.listen}</code> → {fwd.guest_port}/{fwd.proto}{' '}
              </span>
            ))}
          </div>
        )}
        <div className="hint">
          剩余 <b>{remain}</b> · 到期 {ts(o.expires_at)}
          {o.note ? ' · ' + o.note : ''}
        </div>
        {o.err && <div className="err" style={{ marginTop: 4 }}>{o.err}</div>}
        <div className="btnrow">
          {actions.map(([label, fn]: any) => (
            <Btn key={label} variant="ghost" tiny onClick={fn}>
              {label}
            </Btn>
          ))}
        </div>
      </div>
    )
  }

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>云主机</h1>
        <p>卖方发布虚拟机配置，买方下单后卖方机器自动用 QEMU 拉起镜像；访问走卖方的公网 IPv6。</p>
      </div>

      {creds && (
        <Card kicker="Credentials" title="租用凭据（只显示一次）">
          <pre className="code">{creds}</pre>
          <div className="hint" style={{ marginTop: 8 }}>请立即保存私钥与口令；关闭本页后不会再次展示。</div>
        </Card>
      )}

      <Card kicker="Market" title="可租用的配置">
        {!offers ? (
          <Loader />
        ) : others.length === 0 ? (
          <Empty>还没有可租用的配置</Empty>
        ) : (
          others.map((o: any) => (
            <div key={o.id} style={{ paddingTop: 10 }}>
              <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                {o.name}
                {o.available ? <Tag tone="ok">可租用</Tag> : <Tag tone="bad">{o.online ? '已停用' : '节点离线'}</Tag>}
              </h3>
              <div className="tags">
                <Tag>{o.cpus}C / {memGB(o.mem_mb)} / {o.disk_gb}G</Tag>
                <Tag>{o.net_mode}</Tag>
                <Tag>{f2(o.price_hour)} 点/小时</Tag>
              </div>
              <div className="hint" style={{ marginTop: 6 }}>
                {o.os} · {o.image_format} · 最长 {o.max_hours} 小时 · 端口 {portsLabel(o.ports)}
              </div>
              <div className="btnrow">
                <Btn variant="ghost" tiny disabled={!o.available} onClick={() => startRent(o)}>
                  租用
                </Btn>
              </div>
            </div>
          ))
        )}
      </Card>

      <Card kicker="Mine" title="我的云主机">
        {!instances ? <Loader /> : instances.length === 0 ? <Empty>还没有租用记录</Empty> : instances.map((i: any) => offerCard(i))}
      </Card>

      <Card kicker="Host" title="一键把这台机器接入云主机">
        <div className="hint">下载脚本 → 拷到要出租的电脑上运行 → 脚本检查/引导安装 QEMU，再启动 Agent 并绑定账户。启动后刷新本页即可发布配置。</div>
        <div className="row">
          <Field label="机器名称">
            <input className="in" value={launch.vhlname} onChange={(e) => setL('vhlname', e.target.value)} placeholder="例如 宿舍 3060 台式机" />
          </Field>
          <Field label="那台机器的系统">
            <select className="in" value={launch.vhlos} onChange={(e) => setL('vhlos', e.target.value)}>
              <option value="windows">Windows</option>
              <option value="linux">Linux</option>
              <option value="darwin">macOS</option>
            </select>
          </Field>
        </div>
        <div className="row3">
          <Field label="芯片">
            <select className="in" value={launch.vhlarch} onChange={(e) => setL('vhlarch', e.target.value)}>
              <option value="amd64">x86_64</option>
              <option value="arm64">arm64</option>
            </select>
          </Field>
          <Field label="并发云主机数">
            <input className="in" type="number" min="1" max="8" value={launch.vhlmax} onChange={(e) => setL('vhlmax', e.target.value)} />
          </Field>
          <Field label="转码并发槽">
            <input className="in" type="number" min="1" value={launch.vhlslots} onChange={(e) => setL('vhlslots', e.target.value)} />
          </Field>
        </div>
        <Field label="转码单价（点/视频秒）">
          <input className="in" type="number" min="0.05" step="0.05" value={launch.vhlprice} onChange={(e) => setL('vhlprice', e.target.value)} />
        </Field>
        {launch.vhlos === 'linux' && (
          <div className="row">
            <Field label="Linux 网桥（可选）">
              <input className="in" value={launch.vhlbridge} onChange={(e) => setL('vhlbridge', e.target.value)} placeholder="留空用 slirp" />
            </Field>
            <Field label="上联网卡（可选）">
              <input className="in" value={launch.vhlup} onChange={(e) => setL('vhlup', e.target.value)} placeholder="例如 eth0" />
            </Field>
          </div>
        )}
        <div className="btnrow">
          <Btn block onClick={launchVMHost} disabled={busy}>
            下载云主机启动脚本
          </Btn>
        </div>
        {launchMsg && <div className="hint" style={{ marginTop: 8 }}>{launchMsg}</div>}
        <div className="hint" style={{ marginTop: 8 }}>
          云主机需要放行 IPv6 入站 TCP 5900-5999（控制台）与 20000-29999（SSH/自动端口）。Windows 需启用 Windows Hypervisor Platform，Linux 需可读写 /dev/kvm。
        </div>
      </Card>

      <Card kicker="Sell" title={`发布我的虚拟机配置${myOffers.length ? `（${myOffers.length}）` : ''}`}>
        {ownedNodes.length === 0 ? (
          <Empty>还没有支持虚拟机的节点。先用上面的「一键接入」生成启动脚本。</Empty>
        ) : (
          <>
            <div className="btnrow">
              <Btn variant="ghost" tiny onClick={() => setShowPublish((v) => !v)}>
                {showPublish ? '收起表单' : '发布新配置'}
              </Btn>
            </div>
            {showPublish && (
              <>
                <Field label="出租节点">
                  <select className="in" value={pub.node_id} onChange={(e) => setP('node_id', e.target.value)}>
                    {onlineOwned.map((w: any) => (
                      <option key={w.id} value={w.id}>
                        {w.name} · {w.cpus} 核 / {memGB(w.mem_mb)} · {w.vm?.accel || ''}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field label="配置名称">
                  <input className="in" value={pub.name} onChange={(e) => setP('name', e.target.value)} placeholder="例如 2C2G · Ubuntu 24.04" />
                </Field>
                <Field label="系统镜像 URL">
                  <input className="in" value={pub.image_url} onChange={(e) => setP('image_url', e.target.value)} placeholder="https://…/noble-server-cloudimg-amd64.img" />
                </Field>
                <div className="row">
                  <Field label="镜像格式">
                    <select className="in" value={pub.image_format} onChange={(e) => setP('image_format', e.target.value)}>
                      <option value="qcow2">qcow2</option>
                      <option value="raw">raw</option>
                      <option value="iso">iso</option>
                    </select>
                  </Field>
                  <Field label="SHA-256（可选）">
                    <input className="in" value={pub.sha} onChange={(e) => setP('sha', e.target.value)} />
                  </Field>
                </div>
                <div className="row3">
                  <Field label="系统">
                    <input className="in" value={pub.os} onChange={(e) => setP('os', e.target.value)} />
                  </Field>
                  <Field label="vCPU">
                    <input className="in" type="number" min="1" value={pub.cpus} onChange={(e) => setP('cpus', e.target.value)} />
                  </Field>
                  <Field label="内存 MiB">
                    <input className="in" type="number" min="256" step="256" value={pub.mem} onChange={(e) => setP('mem', e.target.value)} />
                  </Field>
                </div>
                <div className="row">
                  <Field label="磁盘 GiB">
                    <input className="in" type="number" min="1" value={pub.disk} onChange={(e) => setP('disk', e.target.value)} />
                  </Field>
                  <Field label="卖方公网 IPv6">
                    <select className="in" value={pub.host_ipv6} onChange={(e) => setP('host_ipv6', e.target.value)}>
                      {nodeIPv6.length === 0 ? <option value="">（节点没有上报 IPv6）</option> : nodeIPv6.map((ip: string) => <option key={ip} value={ip}>{ip}</option>)}
                    </select>
                  </Field>
                </div>
                <Field label="网络模式">
                  <select className="in" value={pub.net_mode} onChange={(e) => setP('net_mode', e.target.value)}>
                    <option value="slirp">slirp · Agent 做 IPv6 端口转发（推荐）</option>
                    <option value="routed">routed · 虚拟机直接持有公网 IPv6</option>
                  </select>
                </Field>
                <Field label="端口映射 host:guest（逗号分隔，可加 /udp）">
                  <input className="in" value={pub.ports} onChange={(e) => setP('ports', e.target.value)} placeholder="2200:22, 8080:80" disabled={pub.net_mode === 'routed'} />
                </Field>
                {pub.net_mode === 'routed' && (
                  <div className="row">
                    <Field label="IPv6 前缀">
                      <input className="in" value={pub.prefix} onChange={(e) => setP('prefix', e.target.value)} placeholder="2001:db8:42::/64" />
                    </Field>
                    <Field label="IPv6 网关">
                      <input className="in" value={pub.gateway} onChange={(e) => setP('gateway', e.target.value)} placeholder="2001:db8:42::1" />
                    </Field>
                  </div>
                )}
                <div className="row">
                  <Field label="价格（点/小时）">
                    <input className="in" type="number" min="0.01" step="0.5" value={pub.price} onChange={(e) => setP('price', e.target.value)} />
                  </Field>
                  <Field label="最长租期（小时）">
                    <input className="in" type="number" min="1" max="720" value={pub.maxh} onChange={(e) => setP('maxh', e.target.value)} />
                  </Field>
                </div>
                <div className="btnrow">
                  <Btn block onClick={submitPublish} disabled={busy}>
                    发布配置
                  </Btn>
                </div>
              </>
            )}
            {msg && <div className="hint" style={{ marginTop: 8 }}>{msg}</div>}
            {myOffers.map((o: any) => (
              <div key={o.id} style={{ paddingTop: 12 }}>
                <h3 style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  {o.name} {o.enabled ? <Tag tone="ok">已上架</Tag> : <Tag tone="bad">已下架</Tag>}
                </h3>
                <div className="hint" style={{ marginTop: 4 }}>
                  {o.cpus}C / {memGB(o.mem_mb)} / {o.disk_gb}G · {o.net_mode} · {f2(o.price_hour)} 点/小时 · 最长 {o.max_hours}h
                </div>
                <div className="hint">
                  IPv6 {o.host_ipv6} · 端口 {portsLabel(o.ports)}
                  {o.net_mode === 'routed' ? ` · 前缀 ${o.ipv6_prefix} · 网关 ${o.ipv6_gateway}` : ''}
                </div>
                <div className="btnrow">
                  <Btn variant="ghost" tiny onClick={() => api.vmSetOffer(o.id, !o.enabled).then(load).catch((e: any) => showToast('操作失败：' + e.message, 'bad'))}>
                    {o.enabled ? '下架' : '重新上架'}
                  </Btn>
                  <Btn variant="ghost" tiny onClick={() => api.vmDelOffer(o.id).then(load).catch((e: any) => showToast('删除失败：' + e.message, 'bad'))}>
                    删除
                  </Btn>
                </div>
              </div>
            ))}
          </>
        )}
      </Card>

      <Sheet open={!!renting} title="确认租用" onClose={() => setRenting(null)}>
        {renting && (
          <>
            <div className="hint">
              {renting.name} · {renting.cpus}C / {memGB(renting.mem_mb)} / {renting.disk_gb}G · {f2(renting.price_hour)} 点/小时 · 最长 {renting.max_hours}h
            </div>
            <div className="row">
              <Field label="主机名称">
                <input className="in" value={order.name} onChange={(e) => setOrder({ ...order, name: e.target.value })} />
              </Field>
              <Field label="租期（小时）">
                <input className="in" type="number" min="1" max={renting.max_hours} value={order.hours} onChange={(e) => setOrder({ ...order, hours: e.target.value })} />
              </Field>
            </div>
            <Field label="SSH 公钥（留空由平台生成）">
              <textarea className="in" value={order.key} onChange={(e) => setOrder({ ...order, key: e.target.value })} placeholder="ssh-ed25519 AAAA… 留空则私钥只显示一次" />
            </Field>
            <div className="btnrow">
              <Btn block onClick={submitRent} disabled={busy}>
                确认租用
              </Btn>
            </div>
            {msg && <div className="hint" style={{ marginTop: 8 }}>{msg}</div>}
          </>
        )}
      </Sheet>
    </div>
  )
}
