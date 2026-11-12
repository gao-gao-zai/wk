import React, { useEffect, useRef, useState } from 'react';
import { Alert, Button, Card, InputNumber, Space, Typography } from 'antd';

export default function GrowthConcurrency({ api }) {
  const [value, setValue] = useState(null);
  const [saved, setSaved] = useState(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [revision, setRevision] = useState(0);
  const alive = useRef(false);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);
  useEffect(() => {
    let disposed = false;
    const controller = new AbortController();
    setLoading(true); setError('');
    api('/admin/config', { signal: controller.signal }).then(body => {
      if (disposed) return;
      if (!Number.isInteger(body.growth_concurrency) || body.growth_concurrency < 1 || body.growth_concurrency > 128) throw new Error('unavailable');
      setValue(body.growth_concurrency); setSaved(body.growth_concurrency);
    }).catch(() => {
      if (!disposed) setError('读取成长并发设置失败，请重试。');
    }).finally(() => { if (!disposed) setLoading(false); });
    return () => { disposed = true; controller.abort(); };
  }, [api, revision]);
  const save = async () => {
    if (saving || !Number.isInteger(value) || value < 1 || value > 128) return;
    const submitted = value;
    setSaving(true); setError('');
    try {
      const result = await api('/admin/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ growth_concurrency: submitted }) });
      if (result.ok !== true) throw new Error('unconfirmed');
      if (alive.current) setSaved(submitted);
    } catch {
      if (alive.current) setError('保存成长并发设置失败，未确认更改。请重新读取核对。');
    } finally { if (alive.current) setSaving(false); }
  };
  return <Card size="small" title="成长任务并发">
    <Space direction="vertical" style={{ width: '100%' }}>
      <Space wrap><label htmlFor="growth-concurrency">不同代理 IP 的最大并发账号数</label><InputNumber id="growth-concurrency" aria-label="成长任务并发数量" min={1} max={128} precision={0} value={value} onChange={setValue} disabled={loading || saving} /><Button loading={saving} disabled={loading || value === saved || !Number.isInteger(value) || value < 1 || value > 128} onClick={save}>保存成长并发</Button><Button disabled={loading || saving} onClick={() => setRevision(n => n + 1)}>重新读取并发</Button>{saved !== null && <Typography.Text type="secondary">当前配置 {saved}（默认 16，范围 1–128）</Typography.Text>}</Space>
      <Typography.Text type="secondary">保存后立即生效。相同账号或代理服务器连接 IP 始终串行；未知 IP 保守串行，实际并发受独立 IP 数和可用端口限制。调小上限不会中断已准入任务，待它们完成后按新值继续。端口不足时保留业务绑定和测速余量，并明确报告任务失败。</Typography.Text>
      {error && <Alert type="warning" showIcon message={error} />}
    </Space>
  </Card>;
}
