import React, { useEffect, useState } from 'react'
import { copyText } from './api'

export function Card({ kicker, title, children, actions, onClick }: any) {
  return (
    <section className={'card' + (onClick ? ' tap' : '')} onClick={onClick}>
      {(title || kicker) && (
        <h2>
          {kicker && <span className="kicker">{kicker}</span>}
          {title}
          {actions && <span style={{ marginLeft: 'auto' }}>{actions}</span>}
        </h2>
      )}
      {children}
    </section>
  )
}

export function Field({ label, hint, children }: any) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint && <div className="hint">{hint}</div>}
    </label>
  )
}

export function Btn({ variant = 'primary', block, tiny, children, ...rest }: any) {
  return (
    <button className={'btn' + (variant !== 'primary' ? ' ' + variant : '') + (block ? ' block' : '') + (tiny ? ' tiny' : '')} {...rest}>
      {children}
    </button>
  )
}

export function Tag({ tone = '', children }: any) {
  return <span className={'tag' + (tone ? ' ' + tone : '')}>{children}</span>
}

export function Stats({ items }: { items: { v: any; k: string; hl?: boolean }[] }) {
  return (
    <div className="stats">
      {items.map((it, i) => (
        <div key={i} className={'stat' + (it.hl ? ' hl' : '')}>
          <b>{it.v}</b>
          <span>{it.k}</span>
        </div>
      ))}
    </div>
  )
}

export function Bar({ pct }: { pct: number }) {
  return (
    <div className="bar">
      <i style={{ width: Math.max(2, Math.min(100, pct || 0)) + '%' }} />
    </div>
  )
}

export function Empty({ children }: any) {
  return <div className="empty">{children}</div>
}

export function Sheet({ open, title, onClose, children }: any) {
  useEffect(() => {
    if (!open) return
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = prev
    }
  }, [open])
  if (!open) return null
  return (
    <>
      <div className="backdrop" onClick={onClose} />
      <div className="sheet" role="dialog" aria-modal="true">
        <div className="sheet-head">
          <h2>{title}</h2>
          <button onClick={onClose}>关闭</button>
        </div>
        {children}
      </div>
    </>
  )
}

export function CopyBlock({ text, label = '复制' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <div>
      <pre className="code">{text}</pre>
      <div className="btnrow">
        <Btn
          variant="ghost"
          tiny
          onClick={async () => {
            setDone(await copyText(text))
            setTimeout(() => setDone(false), 1600)
          }}
        >
          {done ? '已复制' : label}
        </Btn>
      </div>
    </div>
  )
}

export function Loader({ children = '加载中…' }: any) {
  return (
    <div className="empty">
      <span className="spin" /> <span style={{ marginLeft: 6 }}>{children}</span>
    </div>
  )
}
