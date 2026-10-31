import React, { useState } from 'react'
import { api, readBase, store, writeBase } from '../api'
import { Btn, Card, Field } from '../ui'
import { f2 } from '../../../shared/index.js'

export default function Auth({ policy, onDone }: any) {
  const [mode, setMode] = useState<'login' | 'reg'>('login')
  const [name, setName] = useState('')
  const [pass, setPass] = useState('')
  const [pass2, setPass2] = useState('')
  const [invite, setInvite] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [showServer, setShowServer] = useState(false)
  const [base, setBase] = useState(readBase())
  const inviteRequired = !!(policy && policy.signup_invite_required)

  const go = async () => {
    if (!name.trim()) return setMsg('请输入用户名')
    if (mode === 'reg' && pass !== pass2) return setMsg('两次密码不一致')
    setBusy(true)
    setMsg('正在校验…')
    try {
      const payload: any = mode === 'reg' ? { name: name.trim(), password: pass, invite: invite.trim() } : { name: name.trim(), password: pass }
      const r = mode === 'reg' ? await api.register(payload.name, payload.password, payload.invite) : await api.login(payload.name, payload.password)
      if (r && r.token) store.set('ss-token', r.token)
      setMsg('')
      onDone(r)
    } catch (e: any) {
      setMsg('失败：' + (e.message || e))
    } finally {
      setBusy(false)
    }
  }

  const note =
    (inviteRequired ? '本部署注册需要邀请码。' : '') +
    (policy && policy.signup_credit > 0
      ? `新用户注册赠送 ${f2(policy.signup_credit)} 算力点用于体验完整链路。`
      : '本注册不赠送算力点。')

  return (
    <div className="auth">
      <Card>
        <div className="mark">星</div>
        <h1>星栈算力</h1>
        <p className="lead">把闲置的机器接进来按量计价，用别人的算力跑任务，按实测用量结算。</p>
        <div className="segmented">
          <button className={mode === 'login' ? 'on' : ''} onClick={() => { setMode('login'); setMsg('') }}>
            登录
          </button>
          <button className={mode === 'reg' ? 'on' : ''} onClick={() => { setMode('reg'); setMsg('') }}>
            注册
          </button>
        </div>
        <Field label="用户名">
          <input className="in" value={name} onChange={(e) => setName(e.target.value)} placeholder="2-24 个字符" autoComplete="username" />
        </Field>
        <Field label="密码">
          <input className="in" type="password" value={pass} onChange={(e) => setPass(e.target.value)} placeholder="至少 8 位" autoComplete={mode === 'reg' ? 'new-password' : 'current-password'} />
        </Field>
        {mode === 'reg' && (
          <>
            <Field label="确认密码">
              <input className="in" type="password" value={pass2} onChange={(e) => setPass2(e.target.value)} />
            </Field>
            {inviteRequired && (
              <Field label="邀请码">
                <input className="in" value={invite} onChange={(e) => setInvite(e.target.value)} placeholder="向部署者索取" />
              </Field>
            )}
          </>
        )}
        <div className="formfoot">
          <Btn block onClick={go} disabled={busy}>
            {busy ? <span className="spin" /> : null}
            {mode === 'reg' ? '注册并登录' : '登录'}
          </Btn>
        </div>
        {msg && <div className="err" style={{ marginTop: 10 }}>{msg}</div>}
        <div className="security">
          口令以 PBKDF2-HMAC-SHA256（20 万次迭代）存储，服务端不保存明文。{note}
          <div style={{ marginTop: 10 }}>
            <button className="hint" style={{ textDecoration: 'underline' }} onClick={() => setShowServer((v) => !v)}>
              服务器地址：{base || '当前域名'}
            </button>
          </div>
          {showServer && (
            <div style={{ marginTop: 10 }}>
              <input className="in" value={base} onChange={(e) => setBase(e.target.value)} placeholder="http://1.2.3.4:8787" />
              <Btn
                variant="ghost"
                tiny
                style={{ marginTop: 8 }}
                onClick={() => {
                  writeBase(base)
                  location.reload()
                }}
              >
                保存并重连
              </Btn>
            </div>
          )}
        </div>
      </Card>
    </div>
  )
}
