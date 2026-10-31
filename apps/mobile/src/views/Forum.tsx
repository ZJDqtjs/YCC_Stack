import React, { useEffect, useState } from 'react'
import { useApp } from '../ctx'
import { Btn, Card, Empty, Field, Loader } from '../ui'
import { ts } from '../../../shared/index.js'

function Media({ list, api }: any) {
  if (!list || list.length === 0) return null
  return (
    <div style={{ display: 'flex', gap: 6, overflowX: 'auto', marginTop: 8 }}>
      {list.map((f: any) =>
        f.kind === 'video' ? (
          <video key={f.id} src={api.mediaUrl(f.id)} controls preload="metadata" style={{ width: 220, borderRadius: 8, flex: '0 0 auto', background: '#000' }} />
        ) : (
          <img key={f.id} src={api.mediaUrl(f.id)} alt={f.name} style={{ width: 132, height: 132, objectFit: 'cover', borderRadius: 8, flex: '0 0 auto', border: '1px solid var(--hair)' }} />
        ),
      )}
    </div>
  )
}

export default function Forum() {
  const { api, showToast } = useApp()
  const [posts, setPosts] = useState<any[] | null>(null)
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [files, setFiles] = useState<FileList | null>(null)
  const [busy, setBusy] = useState(false)
  const [detail, setDetail] = useState<any>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [reply, setReply] = useState('')
  const [image, setImage] = useState<File | null>(null)
  const [showNew, setShowNew] = useState(false)

  const loadPosts = async () => {
    try {
      const d = await api.forumPosts()
      setPosts(d.posts || [])
    } catch (e: any) {
      showToast('读取论坛失败：' + (e.message || e), 'bad')
      setPosts([])
    }
  }

  useEffect(() => {
    loadPosts()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const openPost = async (id: string) => {
    setDetailLoading(true)
    setDetail({ post: { id } })
    try {
      setDetail(await api.forumPost(id))
    } catch (e: any) {
      showToast('读取帖子失败：' + (e.message || e), 'bad')
      setDetail(null)
    } finally {
      setDetailLoading(false)
    }
  }

  const publish = async () => {
    if (!title.trim()) return showToast('标题不能为空', 'bad')
    setBusy(true)
    try {
      const fd = new FormData()
      fd.set('title', title.trim())
      fd.set('body', body.trim())
      if (files) for (let i = 0; i < files.length; i++) fd.append('files', files[i])
      await api.createPost(fd)
      setTitle('')
      setBody('')
      setFiles(null)
      setShowNew(false)
      showToast('已发布')
      await loadPosts()
    } catch (e: any) {
      showToast('发布失败：' + (e.message || e), 'bad')
    } finally {
      setBusy(false)
    }
  }

  const sendComment = async () => {
    if (!detail) return
    if (!reply.trim() && !image) return showToast('写点内容或选张图', 'bad')
    setBusy(true)
    try {
      const fd = new FormData()
      fd.set('body', reply.trim())
      if (image) fd.append('image', image)
      await api.comment(detail.post.id, fd)
      setReply('')
      setImage(null)
      await openPost(detail.post.id)
    } catch (e: any) {
      showToast('发送失败：' + (e.message || e), 'bad')
    } finally {
      setBusy(false)
    }
  }

  if (detail) {
    const p = detail.post || {}
    return (
      <div className="stack">
        <div className="viewhead">
          <button className="hint" onClick={() => setDetail(null)}>
            ← 返回列表
          </button>
          <h1 style={{ marginTop: 8 }}>{p.title || '帖子'}</h1>
          <p>
            {p.author} · {ts(p.created_at)}
          </p>
        </div>
        {detailLoading ? (
          <Loader />
        ) : (
          <>
            <Card>
              {p.body && <div style={{ whiteSpace: 'pre-wrap', color: 'var(--body)' }}>{p.body}</div>}
              <Media list={detail.media} api={api} />
            </Card>
            <Card kicker="Comments" title={`回复（${(detail.comments || []).length}）`}>
              {(detail.comments || []).length === 0 ? (
                <Empty>还没有评论</Empty>
              ) : (
                <div className="list">
                  {detail.comments.map((c: any) => (
                    <div key={c.id} className="listrow" style={{ display: 'block' }}>
                      <div className="name">{c.author}</div>
                      <div className="meta">{ts(c.created_at)}</div>
                      {c.body && <div style={{ marginTop: 4, whiteSpace: 'pre-wrap' }}>{c.body}</div>}
                      <Media list={c.media} api={api} />
                    </div>
                  ))}
                </div>
              )}
              <div style={{ marginTop: 10 }}>
                <textarea className="in" rows={3} value={reply} onChange={(e) => setReply(e.target.value)} placeholder="写下你的看法" />
                <Field label="附图（可选）">
                  <input className="in" type="file" accept="image/*" onChange={(e) => setImage(e.target.files?.[0] || null)} />
                </Field>
                <div className="btnrow">
                  <Btn block onClick={sendComment} disabled={busy}>
                    发表评论
                  </Btn>
                </div>
              </div>
            </Card>
          </>
        )}
      </div>
    )
  }

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>论坛</h1>
        <p>分享经验、提问或吐槽；支持图片与短视频。</p>
      </div>

      <Card kicker="New" title="说点什么">
        {!showNew ? (
          <div className="btnrow" style={{ marginTop: 0 }}>
            <Btn variant="ghost" tiny onClick={() => setShowNew(true)}>
              发帖
            </Btn>
          </div>
        ) : (
          <>
            <Field label="标题">
              <input className="in" maxLength={120} value={title} onChange={(e) => setTitle(e.target.value)} placeholder="标题" />
            </Field>
            <Field label="内容">
              <textarea className="in" rows={4} value={body} onChange={(e) => setBody(e.target.value)} placeholder="分享经验、提问或吐槽" />
            </Field>
            <Field label="图片或视频（最多 6 图 + 1 视频）">
              <input className="in" type="file" accept="image/*,video/*" multiple onChange={(e) => setFiles(e.target.files)} />
            </Field>
            <div className="btnrow">
              <Btn block onClick={publish} disabled={busy}>
                发布
              </Btn>
            </div>
          </>
        )}
      </Card>

      <Card kicker="Posts" title="最新讨论">
        {!posts ? (
          <Loader />
        ) : posts.length === 0 ? (
          <Empty>还没有帖子，来发第一个吧</Empty>
        ) : (
          <div className="list">
            {posts.map((p: any) => (
              <div className="listrow tap" key={p.id} onClick={() => openPost(p.id)}>
                <div className="grow">
                  <div className="name">{p.title}</div>
                  <div className="meta">
                    {p.author} · {ts(p.created_at)} · {p.comments} 评论 · {p.media_count} 附件
                  </div>
                </div>
                <div className="hint">›</div>
              </div>
            ))}
          </div>
        )}
      </Card>
    </div>
  )
}
