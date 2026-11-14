import React, { useCallback, useEffect, useState } from 'react'
import { useApp } from '../store.jsx'
import { Card, Btn, Tag, Empty, SearchBox, Avatar, Field } from '../ui.jsx'
import { api, href, relTime, apiTS } from '../api.js'

const Media = ({ list }) => !list || !list.length ? null : (
  <div className="media">{list.map(f => f.kind === 'video'
    ? <video key={f.id} controls preload="metadata" src={href('/api/forum/media/' + f.id)} />
    : <img key={f.id} src={href('/api/forum/media/' + f.id)} alt={f.name} loading="lazy" onClick={() => window.open(href('/api/forum/media/' + f.id), '_blank')} />)}</div>
)
export default function Forum() {
  const { toast, prefill, setPrefill } = useApp()
  const [posts, setPosts] = useState([])
  const [q, setQ] = useState('')
  const [title, setTitle] = useState('')
  useEffect(() => {
    if (prefill && prefill.view === 'forum') { setTitle((prefill.tpl && prefill.tpl.title) || ''); setPrefill(null) }
  }, [prefill, setPrefill])
  const [body, setBody] = useState('')
  const [files, setFiles] = useState(null)
  const [msg, setMsg] = useState('')
  const [detail, setDetail] = useState(null)
  const [cBody, setCBody] = useState('')
  const [cImg, setCImg] = useState(null)
  const [cMsg, setCMsg] = useState('')
  const load = useCallback(async () => { try { const d = await api('/api/forum/posts'); setPosts(d.posts || []) } catch (e) {} }, [])
  useEffect(() => { load(); const iv = setInterval(load, 10000); return () => clearInterval(iv) }, [load])
  const filtered = q.trim() ? posts.filter(p => [p.title, p.author].some(x => String(x || '').toLowerCase().includes(q.trim().toLowerCase()))) : posts
  const publish = async () => {
    if (!title.trim()) { setMsg('标题不能为空'); return }
    const fd = new FormData(); fd.set('title', title.trim()); fd.set('body', body.trim())
    if (files) for (const f of files) fd.append('files', f)
    setMsg('发布中…')
    try {
      const r = await fetch(href('/api/forum/posts'), { method: 'POST', body: fd })
      const t = await r.text(); let d = null; try { d = JSON.parse(t) } catch (e) {}
      if (!r.ok) throw (d && d.error) || t
      setTitle(''); setBody(''); setFiles(null); setMsg(''); toast('帖子已发布', 'ok'); load()
    } catch (e) { setMsg('失败：' + e) }
  }
  const open = async id => {
    setDetail({ loading: true })
    try { const d = await api('/api/forum/posts/' + id); setDetail(d) } catch (e) { setDetail(null); toast(String(e), 'bad') }
  }
  const comment = async () => {
    if (!detail || !detail.post) return
    if (!cBody.trim() && !cImg) { setCMsg('写点内容或选张图'); return }
    const fd = new FormData(); fd.set('body', cBody.trim()); if (cImg) fd.append('image', cImg)
    setCMsg('发送中…')
    try {
      const r = await fetch(href('/api/forum/posts/' + detail.post.id + '/comments'), { method: 'POST', body: fd })
      const t = await r.text(); let d = null; try { d = JSON.parse(t) } catch (e) {}
      if (!r.ok) throw (d && d.error) || t
      setCBody(''); setCImg(null); setCMsg(''); open(detail.post.id)
    } catch (e) { setCMsg('失败：' + e) }
  }
  return (
    <div>
      <div className="phead"><div className="phead-txt"><span className="eyebrow">Community</span><h1>星栈论坛</h1><p>分享工作流、讨论调度策略、展示作品。发帖支持图片与视频，帖子按最新发布时间排列。</p></div></div>
      {!detail ? (
        <div className="grid">
          <Card kicker="Threads" title="最新讨论" side={<SearchBox value={q} onChange={setQ} placeholder="搜索帖子…" style={{ flex: '0 1 200px', minWidth: 140 }} />}>
            {filtered.length ? filtered.map(p => (
              <div className="postitem" key={p.id} onClick={() => open(p.id)}>
                <Avatar name={p.author} size={34} />
                <div className="p-main"><h3>{p.title}</h3><div className="postmeta">{p.author} · {relTime(p.created_at)}</div></div>
                <div className="p-side"><Tag>{p.comments} 评论</Tag>{p.media_count ? <Tag tone="gpu">{p.media_count} 附件</Tag> : null}</div>
              </div>
            )) : <Empty>{posts.length ? '没有匹配的帖子' : '还没有帖子，来发第一条吧'}</Empty>}
          </Card>
          <div className="sticky">
            <Card kicker="Compose" title="说点什么">
              <Field label="标题"><input maxLength={120} value={title} onChange={e => setTitle(e.target.value)} placeholder="给帖子起个标题" /></Field>
              <Field label="正文"><textarea rows={4} value={body} onChange={e => setBody(e.target.value)} placeholder="分享经验、提问或吐槽" /></Field>
              <Field label="图片或视频（最多 6 张图 + 1 个视频）"><input type="file" accept="image/*,video/*" multiple onChange={e => setFiles(e.target.files)} /></Field>
              <div className="formfoot"><Btn onClick={publish}>发布</Btn><div className="hint">{msg}</div></div>
            </Card>
          </div>
        </div>
      ) : (
        <div>
          <Btn ghost style={{ marginBottom: 14 }} onClick={() => { setDetail(null); load() }}>← 返回列表</Btn>
          {detail.loading ? <Card><Empty>加载中…</Empty></Card> : (
            <>
              <Card>
                <div style={{ display: 'flex', gap: 12, alignItems: 'center', marginBottom: 14 }}>
                  <Avatar name={detail.post.author} size={34} />
                  <div><h2 style={{ marginBottom: 2 }}>{detail.post.title}</h2><div className="postmeta">{detail.post.author} · {apiTS(detail.post.created_at)}</div></div>
                </div>
                {detail.post.body && <div className="postbody">{detail.post.body}</div>}
                <Media list={detail.media} />
              </Card>
              <Card kicker="Replies" title="回复">
                {(detail.comments || []).length ? detail.comments.map((c, i) => (
                  <div className="comment" key={i}>
                    <div className="who2">{c.author} <span className="postmeta">{apiTS(c.created_at)}</span></div>
                    {c.body && <div className="postbody" style={{ margin: '4px 0' }}>{c.body}</div>}
                    <Media list={c.media} />
                  </div>
                )) : <Empty>还没有评论</Empty>}
                <textarea rows={3} value={cBody} onChange={e => setCBody(e.target.value)} placeholder="写下你的看法" />
                <Field label="附图（可选）"><input type="file" accept="image/*" onChange={e => setCImg(e.target.files[0] || null)} /></Field>
                <div className="formfoot"><Btn onClick={comment}>发表评论</Btn><div className="hint">{cMsg}</div></div>
              </Card>
            </>
          )}
        </div>
      )}
    </div>
  )
}