import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Checkbox, Descriptions, Drawer, Empty, Modal, Progress, Select, Space, Switch, Table, Tag, Typography } from 'antd';
import { CloudSyncOutlined } from '@ant-design/icons';
import { freshness, isActive, pollCollection, progress, resourceLabels, stateLabels } from '../lib/collection';

const { Text } = Typography;
const colors = { success: 'green', completed: 'green', failed: 'red', interrupted: 'orange', running: 'blue', backoff: 'orange' };
export const CollectionTag = ({ status }) => <Tag color={colors[status]}>{stateLabels[status] || status}</Tag>;
export const JobProgress = ({ job }) => <div className="collection-progress"><Progress percent={progress(job)} status={isActive(job) ? 'active' : job.failed ? 'exception' : 'normal'} /><Space wrap><span>已处理 {job.done} / {job.total}</span><Tag color="blue">执行中 {job.running}</Tag><Tag color="green">成功 {job.success}</Tag><Tag color="red">失败 {job.failed}</Tag><Tag>跳过 {job.skipped}</Tag><Tag>取消 {job.cancelled}</Tag></Space></div>;

export function CollectionJob({ api, id, onClose, onComplete }) {
  const { message, modal } = App.useApp();
  const [body, setBody] = useState(null), [error, setError] = useState(''), [page, setPage] = useState(1), [status, setStatus] = useState('');
  const completed = useRef(new Set()), callback = useRef(onComplete); callback.current = onComplete;
  useEffect(() => { setPage(1); setStatus(''); setBody(null); }, [id]);
  useEffect(() => {
    if (!id) return;
    return pollCollection(signal => api(`/admin/collection/jobs/${encodeURIComponent(id)}?page=${page}&page_size=25&status=${status}`, { signal }), value => {
      setBody(value); setError('');
      if (!isActive(value.job) && !completed.current.has(id)) { completed.current.add(id); callback.current?.(); }
      return isActive(value.job);
    }, e => setError(e.message));
  }, [api, id, page, status]);
  const cancel = async () => { try { const result = await api(`/admin/collection/jobs/${id}/cancel`, { method: 'POST' }); setBody(current => current ? { ...current, job: result.job } : current); } catch (e) { message.error(e.message); } };
  const retry = () => modal.confirm({ zIndex: 1400, title: '重试失败与中断项？', okText: '开始重试', cancelText: '取消', content: '将再次查询这些账号的对应资源，成功、跳过和取消项不会重放。', onOk: async () => { const result = await api(`/admin/collection/jobs/${id}/retry`, { method: 'POST' }); window.dispatchEvent(new CustomEvent('collection-started', { detail: result.job_id })); message.success('重试任务已创建，可在采集中心查看'); onClose(); } });
  return <Drawer title="采集任务详情" open={!!id} onClose={onClose} width={900} rootClassName="collection-drawer">
    {error && <Alert type="error" showIcon message={error} />}
    {body ? <><Space wrap><CollectionTag status={body.job.status} /><Text copyable>{id}</Text><Tag>{body.job.source === 'manual' ? '管理员发起' : body.job.source === 'startup' ? '启动采集' : body.job.source === 'action' ? '操作后采集' : '定时采集'}</Tag></Space><JobProgress job={body.job} /><Space wrap style={{ margin: '16px 0' }}>{isActive(body.job) ? <Button onClick={cancel} disabled={body.job.status === 'cancelling'}>取消尚未开始项</Button> : <Button disabled={!body.job.failed} onClick={retry}>重试失败项</Button>}<Select aria-label="采集结果筛选" value={status} onChange={v => { setStatus(v); setPage(1); }} options={[{ value: '', label: '全部结果' }, ...['queued', 'running', 'backoff', 'success', 'failed', 'skipped', 'cancelled', 'interrupted'].map(value => ({ value, label: stateLabels[value] }))]} /></Space>
      <Alert type="info" showIcon message="任务在服务器执行，离开页面后继续。取消只停止尚未开始项；服务重启后未完成项会标记为中断。" />
      <Table rowKey={i => `${i.uid}/${i.resource}`} size="small" scroll={{ x: 650 }} dataSource={body.items} pagination={{ current: page, total: body.total_items, pageSize: 25, showSizeChanger: false, onChange: setPage }} columns={[
        { title: '账号 UID', dataIndex: 'uid', ellipsis: true, width: 200 }, { title: '资源', dataIndex: 'resource', render: r => resourceLabels[r], width: 120 }, { title: '状态', dataIndex: 'status', render: s => <CollectionTag status={s} />, width: 110 }, { title: '尝试', dataIndex: 'attempts', width: 70 }, { title: '结果 / 原因', render: (_, i) => i.error || (i.next_retry_at ? `下次重试 ${new Date(i.next_retry_at * 1000).toLocaleTimeString()}` : '—') },
      ]} /></> : <Empty description={error ? '无法读取任务' : '正在读取任务…'} />}
  </Drawer>;
}

export function CollectionLaunch({ api, uids = [], title = '全部账号', initialResources = ['balance'], onComplete, children, disabled }) {
  const { message } = App.useApp();
  const [open, setOpen] = useState(false), [resources, setResources] = useState(initialResources), [includeDisabled, setIncludeDisabled] = useState(false), [busy, setBusy] = useState(false), [jobID, setJobID] = useState('');
  const submit = async () => {
    setBusy(true);
    try { const result = await api('/admin/collection/jobs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ uids, resources, include_disabled: includeDisabled }) }); setJobID(result.job_id); setOpen(false); window.dispatchEvent(new CustomEvent('collection-started', { detail: result.job_id })); }
    catch (e) { message.error(e.message); } finally { setBusy(false); }
  };
  return <><Button aria-label={typeof children === 'string' ? children : '在线采集'} icon={<CloudSyncOutlined />} disabled={disabled} onClick={() => setOpen(true)}>{children || '在线采集'}</Button>
    <Modal zIndex={1350} title="在线采集上游数据" open={open} onCancel={() => setOpen(false)} onOk={submit} confirmLoading={busy} okText="开始采集" cancelText="取消" okButtonProps={{ disabled: !resources.length }}>
      <p>范围：<strong>{title}</strong>{uids.length > 0 && `（${uids.length} 个账号）`}</p>
      <Checkbox.Group value={resources} onChange={setResources} options={Object.entries(resourceLabels).map(([value, label]) => ({ value, label }))} />
      <div style={{ margin: '20px 0' }}><Space><Switch aria-label="包含禁用账号" checked={includeDisabled} onChange={setIncludeDisabled} /><span>包含禁用账号（仅查询，不启用）</span></Space></div>
      <Alert type="info" showIcon message="会发送真实上游查询。凭证即将到期时会续期并保存，禁用账号仅检查现有凭证。不会签到、领奖或发送对话。" />
    </Modal><CollectionJob api={api} id={jobID} onClose={() => setJobID('')} onComplete={onComplete} /></>;
}

export function CollectionNotice({ api }) {
  const [jobs, setJobs] = useState([]), [error, setError] = useState('');
  useEffect(() => pollCollection(signal => api('/admin/collection', { signal }), b => { setJobs(b.jobs.filter(isActive)); setError(b.storage_error || ''); }, e => setError(e.message), { delay: 5000 }), [api]);
  if (error) return <Alert type="warning" showIcon message={`采集中心：${error}`} />;
  if (!jobs.length) return null;
  const done = jobs.reduce((n, j) => n + j.done, 0), total = jobs.reduce((n, j) => n + j.total, 0);
  return <Alert type="info" showIcon message={<Space wrap><span>上游采集中 · {jobs.length} 个任务 · 已处理 {done} / {total}</span><Button size="small" onClick={() => { location.hash = '#/collection'; }}>查看采集中心</Button></Space>} />;
}

export function AccountSnapshots({ api, uid }) {
  const [snapshots, setSnapshots] = useState([]), [error, setError] = useState('');
  useEffect(() => { setSnapshots([]); return pollCollection(signal => api(`/admin/collection/snapshots?uids=${encodeURIComponent(uid)}&data=1`, { signal }), b => { setSnapshots(b.snapshots); setError(''); }, e => setError(e.message), { delay: 10000 }); }, [api, uid]);
  return <div className="collection-snapshots"><h4>上游数据快照</h4>{error && <Alert type="warning" message={error} />}<Descriptions column={1} bordered size="small" items={snapshots.map(s => ({ key: s.resource, label: resourceLabels[s.resource], children: <Space direction="vertical" size={3}><Space wrap><CollectionTag status={s.state} /><Text type="secondary">{freshness(s)}</Text></Space>{s.last_attempt > 0 && <Text type="secondary">最近尝试 {new Date(s.last_attempt * 1000).toLocaleString()}</Text>}{s.error && <Text type="danger">{s.error}（保留上次成功值）</Text>}{s.resource === 'usage' && s.data && <Text>上次读取 {s.data.pages} 页、{s.data.records} 条记录</Text>}{s.resource === 'models' && Array.isArray(s.data) && <Text>{s.data.map(m => m.ID).join('、') || '无可用模型'}</Text>}{s.resource === 'tasks' && Array.isArray(s.data) && <Text>共 {s.data.length} 项，已领取 {s.data.filter(t => t.claimed).length} 项</Text>}</Space> }))} /></div>;
}
