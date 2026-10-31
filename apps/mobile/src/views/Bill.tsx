import React, { useEffect, useState } from 'react'
import { useApp } from '../ctx'
import { Btn, Card, Empty, Loader, Stats } from '../ui'
import { f2, ts } from '../../../shared/index.js'
import { LEDGER_KIND } from '../../../shared/constants.js'

export default function Bill() {
  const { api, me, policy, showToast } = useApp()
  const [d, setD] = useState<any>(null)
  const [amount, setAmount] = useState('100')
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      setD(await api.ledger())
    } catch (e: any) {
      showToast('读取账单失败：' + (e.message || e), 'bad')
      setD({ spend: [], earn: [] })
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const recharge = async () => {
    const n = Number(amount)
    if (!(n > 0)) return showToast('请输入大于 0 的点数', 'bad')
    setBusy(true)
    try {
      await api.recharge(n)
      showToast('已充值 ' + f2(n) + ' 点')
      await load()
    } catch (e: any) {
      showToast('充值失败：' + (e.message || e), 'bad')
    } finally {
      setBusy(false)
    }
  }

  const spend = d?.spend || []
  const earn = d?.earn || []
  const income = earn.reduce((a: number, e: any) => a + Number(e.amount || 0), 0)
  const outgoing = spend.filter((e: any) => Number(e.amount) < 0).reduce((a: number, e: any) => a + Math.abs(Number(e.amount)), 0)

  return (
    <div className="stack">
      <div className="viewhead">
        <h1>账单</h1>
        <p>账户流水与按节点结算的收益明细；校验不通过的分片不结算。</p>
      </div>

      <Card kicker="Summary" title="账户概览">
        <Stats
          items={[
            { v: f2(me.credits), k: '当前余额', hl: true },
            { v: f2(outgoing), k: '累计支出' },
            { v: f2(income), k: '累计收益' },
          ]}
        />
        {policy && policy.recharge && (
          <>
            <div className="row" style={{ marginTop: 12 }}>
              <input className="in" type="number" min="1" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="充值算力点" />
              <Btn variant="ghost" onClick={recharge} disabled={busy}>
                充值
              </Btn>
            </div>
            <div className="hint" style={{ marginTop: 6 }}>本部署开启了自助充值；公网演练档会关闭该入口。</div>
          </>
        )}
      </Card>

      <Card kicker="Ledger" title="账户流水">
        {!d ? (
          <Loader />
        ) : spend.length === 0 ? (
          <Empty>暂无记录</Empty>
        ) : (
          <div className="list">
            {spend.map((e: any, i: number) => (
              <div className="listrow" key={i}>
                <div className="grow">
                  <div className="name">
                    {LEDGER_KIND[e.kind] || e.kind} {e.job_id ? <span className="tag">{String(e.job_id).slice(0, 8)}</span> : null}
                  </div>
                  <div className="meta">
                    {ts(e.ts)}
                    {e.note ? ' · ' + e.note : ''}
                  </div>
                </div>
                <div className={'amount ' + (Number(e.amount) >= 0 ? 'pos' : 'neg')}>
                  {Number(e.amount) >= 0 ? '+' : ''}
                  {f2(e.amount)}
                  <div className="meta" style={{ textAlign: 'right' }}>余 {f2(e.balance)}</div>
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      <Card kicker="Earnings" title="我的节点收益">
        {!d ? (
          <Loader />
        ) : earn.length === 0 ? (
          <Empty>暂无收益</Empty>
        ) : (
          <div className="list">
            {earn.map((e: any, i: number) => (
              <div className="listrow" key={i}>
                <div className="grow">
                  <div className="name mono">{String(e.node_id || '').slice(0, 14) || '节点'}</div>
                  <div className="meta">
                    {ts(e.ts)}
                    {e.note ? ' · ' + e.note : ''}
                  </div>
                </div>
                <div className="amount pos">+{f2(e.amount)}</div>
              </div>
            ))}
          </div>
        )}
        <div className="hint" style={{ marginTop: 8 }}>收益只在「节点自报哈希 == broker 重算哈希」的分片上发放。</div>
      </Card>
    </div>
  )
}
