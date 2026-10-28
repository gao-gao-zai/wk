import React, { useEffect, useState } from 'react';
import { Alert, App, Button, Card, Form, InputNumber, Space, Switch, Table, Typography } from 'antd';
import { CollectionJob, CollectionLaunch, CollectionTag, JobProgress } from '../components/Collection';
import { isActive, pollCollection, resourceLabels } from '../lib/collection';

export default function CollectionCenter({ api, refresh }) {
  const { message } = App.useApp();
  const [body, setBody] = useState(null), [error, setError] = useState(''), [id, setID] = useState(''), [saving, setSaving] = useState(false), [dirty, setDirty] = useState(false);
  const [form] = Form.useForm();
  useEffect(() => pollCollection(signal => api('/admin/collection', { signal }), b => { setBody(b); setError(''); }, e => setError(e.message)), [api]);
  useEffect(() => { if (body?.settings && !dirty) form.setFieldsValue(body.settings); }, [body?.settings, dirty, form]);
  const save = async values => { setSaving(true); try { const result = await api('/admin/collection/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(values) }); setBody(current => ({ ...current, settings: result.settings })); setDirty(false); message.success('采集设置已保存并生效'); } catch (e) { message.error(e.message); } finally { setSaving(false); } };
  const active = body?.jobs.filter(isActive) || [];
  return <div className="collection-center"><div className="collection-toolbar"><Typography.Text type="secondary">统一采集积分、用量、凭证、模型和成长任务，查看真实进度及数据新鲜度。</Typography.Text><CollectionLaunch api={api} onComplete={refresh}>新建采集</CollectionLaunch></div>
    {error && <Alert type="error" showIcon message={error} />}{body?.storage_error && <Alert type="error" showIcon message={body.storage_error} />}
    {active.map(job => <Card key={job.id} title={<Space><CollectionTag status={job.status} /><span>{job.source === 'manual' ? '手动采集' : '后台采集'}</span></Space>} extra={<Button onClick={() => setID(job.id)}>查看任务</Button>}><JobProgress job={job} /></Card>)}
    <Card title="采集设置"><Form form={form} layout="vertical" onValuesChange={() => setDirty(true)} onFinish={save}><div className="collection-settings-grid"><Form.Item name="concurrency" label="同时采集数" rules={[{ required: true }]}><InputNumber min={1} max={16} /></Form.Item><Form.Item name="requests_per_second" label="每秒最多上游请求" rules={[{ required: true }]}><InputNumber min={1} max={20} /></Form.Item><Form.Item name="automatic" label="启动及定时采集" valuePropName="checked"><Switch /></Form.Item>{Object.entries(resourceLabels).map(([resource, label]) => <Form.Item key={resource} name={['intervals', resource]} label={`${label}周期（秒）`} rules={[{ required: true }]}><InputNumber min={60} max={86400} /></Form.Item>)}</div><Alert type="info" showIcon message="自动采集跳过禁用账号，优先读取过期/未知快照。凭证只在临近到期时续期。并发和速率分别限制；失败保留上次数据。" /><Button htmlType="submit" type="primary" loading={saving} disabled={!body || !dirty} style={{ marginTop: 16 }}>保存设置</Button></Form></Card>
    <Card title="最近任务" extra={<Typography.Text type="secondary">保留最近 100 个历史任务</Typography.Text>}><Table rowKey="id" size="middle" scroll={{ x: 700 }} dataSource={body?.jobs || []} pagination={{ pageSize: 10, showSizeChanger: false }} columns={[
      { title: '创建时间', dataIndex: 'created_at', render: v => new Date(v * 1000).toLocaleString(), width: 180 }, { title: '来源', dataIndex: 'source', render: s => ({ manual: '管理员', scheduled: '定时', startup: '启动', action: '操作后' }[s] || s), width: 90 }, { title: '状态', dataIndex: 'status', render: s => <CollectionTag status={s} />, width: 110 }, { title: '处理进度', render: (_, j) => `${j.done} / ${j.total} · 成功 ${j.success} · 失败 ${j.failed} · 跳过 ${j.skipped}` }, { title: '操作', width: 90, render: (_, j) => <Button type="link" onClick={() => setID(j.id)}>查看</Button> },
    ]} /></Card><CollectionJob api={api} id={id} onClose={() => setID('')} onComplete={refresh} />
  </div>;
}
