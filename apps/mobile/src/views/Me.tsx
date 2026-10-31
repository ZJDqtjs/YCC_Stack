import React, { useEffect, useState } from 'react'
import { readBase, writeBase } from '../api'
import { useApp } from '../ctx'
import { Btn, Card, Field } from '../ui'
import { f2 } from '../../../shared/index.js'

export default function Me() {
  const { api, me, showToast, signOut } = useApp()
  const [adv, setAdv] = useState(false)
  const [base, setBase] = useState(readBase())

  useEffect(() => {
    api
      .settings()
      .then((d: any) => setAdv(!!d.advanced))
      .catch(() => {})
  }, [api])

  const toggleAdv = async (on: boolean) => {
    setAdv(on)
    try {
      const r = await api.saveSettings(on)
      setAdv(!!r.advanced)
      showToast(on ? '已打开高级选项' : '已切换为简洁模式', '')
    } catch (e: any) {
      setAdv(!on)
      showToast('保存失败：' + (e.message || e), 'bad')
    }
  }

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>个人资料</h1>
      </div>

      <Card>
        <div className="row">
          <div>
            <div className="kicker">账户</div>
            <div style={{ fontSize: 18, fontWeight: 600, color: 'var(--ink)', marginTop: 4 }}>{me.name}</div>
          </div>
          <div>
            <div className="kicker">余额</div>
            <div style={{ fontSize: 18, fontWeight: 600, color: 'var(--ink)', marginTop: 4 }}>{f2(me.credits)} 点</div>
          </div>
        </div>
        <div className="btnrow">
          <Btn variant="ghost" tiny onClick={() => signOut()}>
            退出登录
          </Btn>
        </div>
      </Card>

      <Card kicker="设置" title="界面">
        <label className="check">
          <input type="checkbox" checked={adv} onChange={(e) => toggleAdv(e.target.checked)} />
          显示高级选项（编码器、硬件要求、分片数、服务端路径）
        </label>
        <div className="hint" style={{ marginTop: 8 }}>
          {adv ? '高级选项已显示在提交任务页。' : '简洁模式只保留文件、任务类型和画质；渲染任务仍会展开必要参数。'}
        </div>
      </Card>

      <Card kicker="连接" title="服务器地址">
        <Field label="Broker 根地址" hint="App 在构建时默认指向公网演练环境；换服务器后保存并重连。">
          <input className="in" value={base} onChange={(e) => setBase(e.target.value)} placeholder="http://1.2.3.4:8787" />
        </Field>
        {!base && <div className="hint">当前使用开发代理（同源 /api），打包后默认指向公网演练环境。</div>}
        <div className="btnrow">
          <Btn
            variant="ghost"
            tiny
            onClick={() => {
              writeBase(base)
              location.reload()
            }}
          >
            保存并重连
          </Btn>
        </div>
      </Card>

      <Card kicker="About" title="星栈算力">
        <div className="hint">
          移动端 v1.0 · React + Capacitor。调度与交易中间层，不自研分布式运行时；积分租约、分片调度与计量结算服务端完成。
        </div>
      </Card>
    </div>
  )
}
