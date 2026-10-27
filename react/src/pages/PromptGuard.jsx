import React, { useCallback, useEffect, useState } from 'react';
import { Alert, App, Button, Card, Descriptions, Drawer, Empty, Form, Input, InputNumber, Modal, Select, Space, Statistic, Switch, Table, Tabs, Tag, Typography } from 'antd';
import { ArrowDownOutlined, ArrowUpOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';
import './PromptGuard.css';

const { Text, Paragraph } = Typography;
const root = '/admin/prompt-guard/';
const time = value => value ? new Date(value).toLocaleString() : '—';
const states = { running: '诊断中', queued: '排队中', located: '已定位', passed: '验证通过', blocked: '仍被拒绝', inconclusive: '结果不确定', cancelled: '已取消', interrupted: '已中断' };
const outcomes = { pass: '通过', blocked: '11128 拒绝', inconclusive: '不确定' };
const json = value => ({ headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(value) });
const changed = () => window.dispatchEvent(new Event('prompt-guard-changed'));
const textOf = messages => (messages || []).map(m => typeof m.content === 'string' ? m.content : (m.content || []).filter(p => ['text', 'input_text'].includes(p.type)).map(p => p.text || '').join('\n')).join('\n\n');

export function PromptGuardNotice({ api, onOpen }) {
  const [summary, setSummary] = useState(null);
  useEffect(() => {
    let live = true;
    const refresh = () => api(`${root}summary`).then(v => { if (live) setSummary(v); }).catch(() => { if (live) setSummary(null); });
    refresh(); const timer = setInterval(refresh, 30000);
    window.addEventListener('prompt-guard-changed', refresh);
    return () => { live = false; clearInterval(timer); window.removeEventListener('prompt-guard-changed', refresh); };
  }, [api]);
  if (!summary?.pending && !summary?.storage_error) return null;
  return <Alert className="pg-notice" type="warning" showIcon message={summary.storage_error || `${summary.pending} 条系统提示词拦截记录待处理`} description="检查触发片段，设置替换规则，并验证完整系统提示词。" action={<Button onClick={onOpen}>查看待处理</Button>} />;
}

function PromptText({ title, messages }) {
  return <section className="pg-prompt"><Text strong>{title}</Text>{(messages || []).map((m, i) => <div key={i}><Tag>{m.role} · {i + 1}</Tag><pre>{typeof m.content === 'string' ? m.content : JSON.stringify(m.content, null, 2)}</pre></div>)}{!messages?.length && <Text type="secondary">原请求没有系统提示词</Text>}</section>;
}

export default function PromptGuardPage({ api }) {
  const { message, modal } = App.useApp();
  const [config, setConfig] = useState(null);
  const [settings, setSettings] = useState(null);
  const [cases, setCases] = useState([]);
  const [tab, setTab] = useState('cases');
  const [filter, setFilter] = useState('pending');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [detailId, setDetailId] = useState(null);
  const [detail, setDetail] = useState(null);
  const [preview, setPreview] = useState(null);
  const [editor, setEditor] = useState(null);
  const [ruleForm] = Form.useForm();

  const load = useCallback(async () => {
    try {
      const [c, list] = await Promise.all([api(`${root}config`), api(`${root}cases`)]);
      setConfig(c); setSettings(current => current || c.settings); setCases(list.cases || []); setError('');
    } catch (e) { setError(e.message); }
  }, [api]);
  useEffect(() => { load(); const timer = setInterval(load, 5000); return () => clearInterval(timer); }, [load]);
  useEffect(() => {
    if (!detailId) { setDetail(null); return undefined; }
    let live = true; setDetail(null); setPreview(null);
    const refresh = () => api(`${root}cases/${detailId}`).then(v => { if (live) setDetail(v); }).catch(e => { if (live) setError(e.message); });
    refresh(); const timer = setInterval(refresh, 3000);
    return () => { live = false; clearInterval(timer); };
  }, [api, detailId]);

  const saveConfig = async patch => {
    setBusy(true);
    try { const c = await api(`${root}config`, { method: 'PUT', ...json({ ...config, ...patch }) }); setConfig(c); changed(); message.success('配置已保存并立即生效'); return c; }
    catch (e) { setError(e.message); message.error(e.message); return null; }
    finally { setBusy(false); }
  };
  const edit = rule => { setEditor(rule || {}); ruleForm.setFieldsValue({ name: '', match: '', replacement: '', enabled: true, ...rule }); };
  const saveRule = async () => {
    const values = await ruleForm.validateFields().catch(() => null); if (!values) return;
    const rules = [...(config.rules || [])];
    const i = rules.findIndex(r => r.id === editor.id);
    const rule = { ...editor, ...values };
    if (i < 0) rules.push(rule); else rules[i] = rule;
    if (await saveConfig({ rules })) setEditor(null);
  };
  const move = async (index, direction) => { const rules = [...config.rules]; [rules[index], rules[index + direction]] = [rules[index + direction], rules[index]]; await saveConfig({ rules }); };
  const launch = kind => modal.confirm({
    title: kind === 'locate' ? '开始二分定位？' : '验证替换后的完整系统提示词？',
    content: <div><Paragraph>将向该记录的上游模型 {detail?.model} 发送真实请求，可能产生用量。</Paragraph><Paragraph>{kind === 'locate' ? `最多 ${config?.settings.probe_budget} 次，先验证对照与按当前规则转换的完整系统提示词，再检查句子及句子组合。` : '最多 2 次：先发送对照，再发送经当前全部替换和兼容处理的完整系统提示词。'}</Paragraph><Paragraph>只使用已保存的系统提示词和固定的“Reply with OK.”用户消息。历史业务对话不参与测试。可随时取消。</Paragraph></div>,
    okText: '确认发送', cancelText: '取消',
    onOk: async () => {
      try { await api(`${root}cases/${detailId}/${kind}`, { method: 'POST' }); setDetail(await api(`${root}cases/${detailId}`)); changed(); message.success('诊断已启动'); }
      catch (e) { message.error(e.message); throw e; }
    },
  });
  const cancel = async () => { try { await api(`${root}cases/${detailId}/cancel`, { method: 'POST' }); message.success('已请求取消'); await load(); } catch (e) { message.error(e.message); } };
  const remove = row => modal.confirm({ title: '删除这条拦截记录？', content: '将删除保存的完整系统提示词和诊断结果；后续新的 11128 仍会记录。', okText: '删除', okButtonProps: { danger: true }, cancelText: '取消', onOk: async () => { await api(`${root}cases/${row.id}`, { method: 'DELETE' }); if (row.id === detailId) setDetailId(null); await load(); changed(); } });
  const rulesColumns = [
    { title: '顺序', width: 100, render: (_, r, i) => <Space><Text>{i + 1}</Text><Button aria-label="上移规则" size="small" disabled={busy || i === 0} icon={<ArrowUpOutlined />} onClick={() => move(i, -1)} /><Button aria-label="下移规则" size="small" disabled={busy || i === config.rules.length - 1} icon={<ArrowDownOutlined />} onClick={() => move(i, 1)} /></Space> },
    { title: '规则', dataIndex: 'name', render: (v, r) => <div><Text strong>{v || '未命名规则'}</Text><div className="pg-snippet">{r.match}</div></div> },
    { title: '替换为', dataIndex: 'replacement', render: v => <div className="pg-snippet">{v || <Text type="secondary">删除匹配内容</Text>}</div> },
    { title: '启用', dataIndex: 'enabled', width: 80, render: (v, r) => <Switch aria-label={`启用 ${r.name || '规则'}`} checked={v} disabled={busy} onChange={enabled => saveConfig({ rules: config.rules.map(rule => rule.id === r.id ? { ...rule, enabled } : rule) })} /> },
    { title: '操作', width: 140, render: (_, r) => <Space><Button size="small" icon={<EditOutlined />} disabled={busy} onClick={() => edit(r)}>编辑</Button><Button size="small" danger disabled={busy} onClick={() => modal.confirm({ title: '删除替换规则？', okText: '删除', cancelText: '取消', onOk: () => saveConfig({ rules: config.rules.filter(rule => rule.id !== r.id) }) })}>删除</Button></Space> },
  ];
  const pending = cases.filter(c => !c.resolved).length;
  const active = cases.filter(c => ['running', 'queued'].includes(c.job?.status)).length;
  const detailActive = ['running', 'queued'].includes(detail?.job?.status);
  return <div className="pg-page">
    {error && <Alert type="error" showIcon message={error} action={<Button onClick={load}>重试</Button>} />}
    <div className="pg-stats"><Card><Statistic title="待处理提示词" value={pending} /></Card><Card><Statistic title="已启用替换规则" value={(config?.rules || []).filter(r => r.enabled).length} /></Card><Card><Statistic title="运行或排队中的诊断" value={active} /></Card><Card><Text type="secondary">自动定位</Text><div className="pg-setting-state"><Tag color={config?.settings.auto_locate ? 'blue' : 'default'}>{config?.settings.auto_locate ? '已开启' : '已关闭'}</Tag><Text type="secondary">完整提示词始终记录</Text></div></Card></div>
    <Card title="系统提示词管理" extra={<Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>}>
      <Tabs activeKey={tab} onChange={setTab} items={[
        { key: 'cases', label: `拦截记录${pending ? ` · ${pending} 待处理` : ''}`, children: <>
          <div className="pg-toolbar"><Text type="secondary">仅记录上游 code=11128，重复提示词合并计数。</Text><Select aria-label="筛选拦截记录" value={filter} onChange={setFilter} options={[{ value: 'pending', label: '待处理' }, { value: 'all', label: '全部记录' }, { value: 'resolved', label: '已验证通过' }]} /></div>
          <Table rowKey="id" dataSource={cases.filter(c => filter === 'all' || (filter === 'resolved' ? c.resolved : !c.resolved))} pagination={{ pageSize: 10 }} scroll={{ x: 850 }} locale={{ emptyText: '暂无对应的 11128 拦截记录' }} columns={[
            { title: '模型 / 记录', dataIndex: 'model', render: (v, r) => <div><Text strong>{v}</Text><div><Text type="secondary">{r.id.slice(0, 12)}</Text></div></div> },
            { title: '最近拦截', dataIndex: 'last_seen', render: time }, { title: '次数', dataIndex: 'occurrences', width: 70 },
            { title: '处理状态', render: (_, r) => <Space direction="vertical" size={2}><Tag color={r.resolved ? 'green' : 'orange'}>{r.resolved ? '验证通过' : '待处理'}</Tag>{r.job && <Text type="secondary">{r.job.status === 'passed' && !r.resolved ? '历史验证已失效' : states[r.job.status]} · {r.job.calls}/{r.job.budget} 次</Text>}</Space> },
            { title: '操作', render: (_, r) => <Space><Button type="link" onClick={() => setDetailId(r.id)}>查看与处理</Button><Button type="text" danger disabled={['running', 'queued'].includes(r.job?.status)} onClick={() => remove(r)}>删除</Button></Space> },
          ]} />
        </> },
        { key: 'rules', label: '替换规则', children: <>
          <Alert type="info" showIcon message="按顺序对每个 system / developer 文本块执行精确、区分大小写的全量替换；后面的规则可以匹配前面的替换结果。" description="支持任意多行内容与空替换，不支持正则。用户消息、工具内容和图片保持原样。通用规则先于已有 DSH / Codex 兼容处理运行。" />
          <div className="pg-toolbar"><Text type="secondary">保存后立即生效。修改规则后，旧验证结果需要重新验证。</Text><Button type="primary" icon={<PlusOutlined />} disabled={!config} onClick={() => edit()}>新增规则</Button></div>
          <Table rowKey="id" columns={rulesColumns} dataSource={config?.rules || []} pagination={false} scroll={{ x: 850 }} locale={{ emptyText: '添加任意需要替换的系统提示词内容' }} />
        </> },
        { key: 'settings', label: '诊断设置', children: settings && <Form layout="vertical" className="pg-settings">
          <Alert type="warning" showIcon message="自动定位会发送真实上游请求" description="开启即允许对今后新捕获的 11128 记录自动诊断，单任务按预算串行请求。重复记录不会反复自动重试。关闭会取消自动任务，完整提示词仍会保存，可手动重新定位。" />
          <Form.Item label="自动二分定位新拦截"><Switch aria-label="自动二分定位" checked={settings.auto_locate} onChange={v => setSettings({ ...settings, auto_locate: v })} /></Form.Item>
          <Form.Item label="每次诊断最多请求次数" extra="包含对照请求和复现检查；达到预算会保留当前片段并标记结果不确定。"><InputNumber aria-label="请求预算" min={4} max={256} value={settings.probe_budget} onChange={v => setSettings({ ...settings, probe_budget: v })} /></Form.Item>
          <Form.Item label="记录保留天数"><InputNumber aria-label="保留天数" min={1} max={365} value={settings.retention_days} onChange={v => setSettings({ ...settings, retention_days: v })} /></Form.Item>
          <Form.Item label="最多保留的记录数" extra="默认 200 条；加密存储总上限 64 MiB，超限会在登录提醒中报告存储错误，提示词不会被截断。"><InputNumber aria-label="记录上限" min={1} max={1000} value={settings.max_cases} onChange={v => setSettings({ ...settings, max_cases: v })} /></Form.Item>
          <Space><Button type="primary" loading={busy} onClick={async () => { const c = await saveConfig({ settings }); if (c) setSettings(c.settings); }}>保存诊断设置</Button><Button onClick={() => setSettings(config.settings)}>撤销未保存设置</Button></Space>
        </Form> },
      ]} />
    </Card>
    <Drawer title="拦截详情与诊断" open={!!detailId} width="min(940px, 100vw)" onClose={() => setDetailId(null)} destroyOnHidden>
      {!detail ? <Empty description="正在加载记录" /> : <Space direction="vertical" size={18} style={{ width: '100%' }}>
        <Descriptions size="small" column={2} items={[{ key: 'model', label: '模型', children: detail.model }, { key: 'status', label: '状态', children: detail.resolved ? '完整提示词已验证通过' : '待处理' }, { key: 'count', label: '拦截次数', children: detail.occurrences }, { key: 'seen', label: '最近拦截', children: time(detail.last_seen) }]} />
        <Space wrap><Button type="primary" disabled={detailActive || active > 0} onClick={() => launch('locate')}>{detail.job ? '重新二分定位' : '开始二分定位'}</Button><Button disabled={detailActive || active > 0} onClick={() => launch('verify')}>验证当前替换</Button><Button onClick={async () => { try { const v = await api(`${root}cases/${detailId}/preview`, { method: 'POST' }); setPreview(v.messages); } catch (e) { message.error(e.message); } }}>本地替换预览</Button>{detailActive && <Button danger onClick={cancel}>取消诊断</Button>}</Space>
        <Alert type="info" showIcon message="验证包含完整系统提示词，但不包含历史用户对话和工具调用；通过只代表本次模型与账号下的这次检测。" />
        {detail.job && <Card size="small" title={<Space><Tag color={detail.resolved ? 'green' : 'blue'}>{detail.job.status === 'passed' && !detail.resolved ? '历史验证已失效' : states[detail.job.status]}</Tag><Text>{detail.job.calls}/{detail.job.kind === 'verify' ? 2 : detail.job.budget} 次请求</Text></Space>}><Paragraph>{detail.job.status === 'passed' && !detail.resolved ? '规则或兼容处理已修改，或该提示词再次被拦截，请重新验证当前替换。' : (detail.job.note || '正在向上游发送诊断请求…')}</Paragraph><Table size="small" rowKey={(_, i) => i} pagination={{ pageSize: 6 }} dataSource={detail.job.trace || []} columns={[{ title: '阶段', dataIndex: 'step', render: v => ({ control: '对照请求', reproduce_full: '完整提示词复现', bisect: '分段 / 组合检查', verify_full: '完整替换验证' }[v] || v) }, { title: '字符数', dataIndex: 'characters' }, { title: '结果', dataIndex: 'outcome', render: v => outcomes[v] || v }, { title: 'HTTP', dataIndex: 'http' }]} /></Card>}
        {detail.candidate && <Card size="small" title={detail.candidate_minimal ? '定位到的句子或句子组合' : '当前可复现片段（尚未完成定位）'} extra={<Button onClick={() => edit({ match: detail.candidate.includes('[…]') ? '' : detail.candidate })}>设为替换规则</Button>}><pre className="pg-candidate">{detail.candidate}</pre><Text type="secondary">若包含 […]，表示不相邻句子的组合，请分别选取原文设置规则。可能还有其他触发片段，需验证完整提示词。</Text></Card>}
        {preview && <PromptText title="按当前规则及兼容处理后的预览（未发送上游）" messages={preview} />}
        <PromptText title="原始完整系统提示词" messages={detail.original} />
        <PromptText title="当时实际出站的完整系统提示词" messages={detail.outbound} />
        {detail.diagnostic && <PromptText title="最近一次定位使用的完整系统提示词" messages={detail.diagnostic} />}
        <Button onClick={() => edit({ match: textOf(detail.original) })}>根据原始提示词新增规则</Button>
      </Space>}
    </Drawer>
    <Modal title={editor?.id ? '编辑替换规则' : '新增替换规则'} open={editor !== null} onCancel={() => setEditor(null)} onOk={saveRule} confirmLoading={busy} width={760} okText="保存规则" cancelText="取消" destroyOnHidden>
      <Form form={ruleForm} layout="vertical"><Form.Item name="name" label="规则名称"><Input maxLength={200} placeholder="例如：路径说明同义改写" /></Form.Item><Form.Item name="match" label="匹配原文（精确匹配）" rules={[{ required: true, message: '请输入要替换的原文' }]}><Input.TextArea rows={6} spellCheck={false} /></Form.Item><Form.Item name="replacement" label="替换内容" extra="留空会删除匹配内容。请自行确认改写前后的语义。"><Input.TextArea rows={6} spellCheck={false} /></Form.Item><Form.Item name="enabled" label="启用规则" valuePropName="checked"><Switch /></Form.Item><Alert type="info" showIcon message="保存规则不会自动验证。保存后可在拦截详情点击“验证当前替换”，确认后发送请求。" /></Form>
    </Modal>
  </div>;
}
