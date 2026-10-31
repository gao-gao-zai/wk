import React, { useCallback, useEffect, useState } from 'react';
import { Alert, App, Button, Card, Descriptions, Drawer, Empty, Form, Input, InputNumber, Modal, Select, Space, Statistic, Switch, Table, Tabs, Tag, Typography } from 'antd';
import { ArrowDownOutlined, ArrowUpOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';
import './PromptGuard.css';
import { accountScopeLabel, candidateView, diagnosticStates, httpDescription, jobView, probeExplanation, requestAttempted, selectionView, stageNames, verificationStages } from '../lib/promptGuard';

const { Text, Paragraph } = Typography;
const root = '/admin/prompt-guard/';
const time = value => value ? new Date(value).toLocaleString() : '—';
const states = diagnosticStates;
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

function DiagnosticJob({ item }) {
  const job = item.job;
  const view = jobView(job);
  const selection=selectionView(job);
  const stale = job.status === 'passed' && !item.resolved;
  const columns = [
    { title: '检查步骤', dataIndex: 'step', width: 160, render: (v, row, i) => <div><Text strong>{i + 1}. {stageNames[v] || v}</Text><div><Text type="secondary">{row.characters} 字符</Text></div></div> },
    { title: '结果 / 请求状态', width: 160, render: (_, row) => <div><Tag color={row.outcome === 'pass' ? 'green' : row.outcome === 'blocked' ? 'red' : 'orange'}>{outcomes[row.outcome] || row.outcome}</Tag><div><Text type="secondary">{row.cached_from ? `复用第 ${row.cached_from} 步，未重复发送` : requestAttempted(row) === false ? '未发送上游请求' : requestAttempted(row) === true ? '已尝试请求上游' : '旧记录未保存发送状态'}</Text></div></div> },
    { title: 'HTTP / 耗时', width: 140, render: (_, row) => <div><Text>{httpDescription(row)}</Text><div><Text type="secondary">{row.cached_from ? '本次无需等待' : row.duration_ms != null ? `${(row.duration_ms / 1000).toFixed(2)} 秒` : '耗时未记录'}</Text></div></div> },
    { title: '具体原因', render: (_, row) => <div><Text>{probeExplanation(row).title}</Text>{row.reason && <div><Text className="pg-reason-code" type="secondary">{row.reason}</Text></div>}</div> },
  ];
  return <Card size="small" className="pg-job" title={<div className="pg-job-heading"><Text strong>{job.kind === 'verify' ? '本次完整提示词替换验证' : '本次二分定位'}</Text><Tag color={stale ? 'orange' : view.type === 'success' ? 'green' : view.active ? 'blue' : 'orange'}>{stale ? '历史验证已失效' : states[job.status] || job.status}</Tag></div>}>
    <Alert type={stale ? 'warning' : view.type} showIcon message={stale ? '旧验证结果已失效，需要重新验证' : view.headline} description={stale ? '规则已变化或该提示词再次被拦截，本次记录仍待处理。' : view.description} />
    <section className="pg-account-selection" aria-label="本次诊断账号"><Text strong>{selection.title}</Text>{selection.uid && <div><Text type="secondary">账号 UID：</Text><Text code copyable>{selection.uid}</Text></div>}{selection.scope && <div><Tag>{selection.scope}</Tag><Text type="secondary">范围内 {selection.scopeCount} 个账号，选号时可用 {selection.availableCount} 个{selection.waitMS!=null?`，选号耗时 ${(selection.waitMS/1000).toFixed(2)} 秒`:''}</Text></div>}<Paragraph type="secondary">{selection.description}</Paragraph></section>
    <div className="pg-job-metrics"><div><Text type="secondary">上游请求尝试</Text><strong>{view.requestLabel}</strong></div><div><Text type="secondary">已执行检查</Text><strong>{view.checks} 步</strong></div><div><Text type="secondary">复用重复结果</Text><strong>{view.cached} 次</strong></div></div>
    <Text type="secondary">本次最多 {view.budget} 次请求，上限不是必须发送的数量；对照失败时会停止后续检查。复用结果不消耗额外请求预算。</Text>
    <div className="pg-stages" role="list" aria-label="诊断阶段">{verificationStages(job).map((stage, i) => <div className="pg-stage" role="listitem" key={stage.step}><div><Text strong>{i + 1}. {stage.name}</Text><Tag color={stage.outcome === 'pass' ? 'green' : stage.outcome === 'blocked' ? 'red' : 'default'}>{stage.state}</Tag></div><Text type="secondary">{stage.explanation}</Text></div>)}</div>
    {job.kind === 'locate' && <Paragraph type="secondary">通过对照并复现完整提示词后，才会开始分段与组合检查。</Paragraph>}
    <Table className="pg-trace" size="small" rowKey={(_, i) => i} pagination={(job.trace || []).length > 6 ? { pageSize: 6, showSizeChanger: false } : false} scroll={{ x: 720 }} dataSource={job.trace || []} columns={columns} locale={{ emptyText: view.active ? '正在准备第一项检查…' : '本次未执行任何检查' }} />
    <div className="pg-job-time"><Text type="secondary">开始：{time(job.started)}</Text>{job.finished && <Text type="secondary">结束：{time(job.finished)}</Text>}</div>
  </Card>;
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
  const edit = rule => { setEditor(rule || {}); ruleForm.setFieldsValue({ name: '', match: '', replacement: '', enabled: true, require_end_boundary: false, ...rule }); };
  const saveRule = async () => {
    const values = await ruleForm.validateFields().catch(() => null); if (!values) return;
    const rules = [...(config.rules || [])];
    const i = rules.findIndex(r => r.id === editor.id);
    const rule = { ...editor, ...values };
    if (i < 0) rules.push(rule); else rules[i] = rule;
    if (await saveConfig({ rules })) setEditor(null);
  };
  const move = async (index, direction) => { const rules = [...config.rules]; [rules[index], rules[index + direction]] = [rules[index + direction], rules[index]]; await saveConfig({ rules }); };
  const launch = async kind => {
    let override=null;
    let groups=[];
    if(!detail?.account_scope) { try {const result=await api('/admin/groups');groups=result.groups || [];} catch {} }
    return modal.confirm({
    title: kind === 'locate' ? '开始二分定位？' : '验证替换后的完整系统提示词？',
    content: <div><Paragraph>将向模型 {detail?.model} 发送真实请求，可能产生用量。</Paragraph><Paragraph>选号范围：{accountScopeLabel(detail?.account_scope)}。启动时选择可用账号，本次任务固定使用，结束或取消时释放槽位。</Paragraph>{!detail?.account_scope && <div className="pg-scope-choice"><Alert type="warning" showIcon message="旧记录没有保存原请求分组" description="默认仅使用原账号。管理员可明确选择本次允许的分组；选择全池表示授权本次诊断使用全池账号。" /><Select aria-label="本次诊断选号范围" defaultValue="legacy" style={{width:'100%',margin:'12px 0'}} options={[{value:'legacy',label:'仅原账号（保守默认）'},...groups.map(g=>({value:`group:${g.name}`,label:`分组：${g.name}`})),{value:'all',label:'管理员全池（明确授权本次使用）'}]} onChange={value=>{override=value==='legacy'?null:value==='all'?{kind:'all'}:{kind:'group',group:value.slice(6)};}} /></div>}<Paragraph>{kind === 'locate' ? `最多 ${config?.settings.probe_budget} 次，先验证对照与按当前规则转换的完整系统提示词，再检查句子及句子组合。同一任务内重复片段复用已确定的结果。` : '最多 2 次：先检查中性对照，对照通过后才验证经当前替换处理的完整系统提示词。'}</Paragraph><Paragraph>范围内没有可用账号、所选账号失效或对照失败时会停止，不会中途换号。请求上限不是必须发送的数量。</Paragraph><Paragraph>只使用已保存的系统提示词和固定的“Reply with OK.”用户消息。历史业务对话不参与测试。可随时取消。</Paragraph></div>,
    okText: '确认发送', cancelText: '取消',
    onOk: async () => {
      try { await api(`${root}cases/${detailId}/${kind}`, { method: 'POST',...(override?json({account_scope:override}):{}) }); setDetail(await api(`${root}cases/${detailId}`)); changed(); message.success('诊断已启动'); }
      catch (e) { message.error(e.message); throw e; }
    },
  }); };
  const cancel = async () => { try { await api(`${root}cases/${detailId}/cancel`, { method: 'POST' }); message.success('已请求取消'); await load(); } catch (e) { message.error(e.message); } };
  const remove = row => modal.confirm({ title: '删除这条拦截记录？', content: '将删除保存的完整系统提示词和诊断结果；后续新的 11128 仍会记录。', okText: '删除', okButtonProps: { danger: true }, cancelText: '取消', onOk: async () => { await api(`${root}cases/${row.id}`, { method: 'DELETE' }); if (row.id === detailId) setDetailId(null); await load(); changed(); } });
  const rulesColumns = [
    { title: '顺序', width: 100, render: (_, r, i) => <Space><Text>{i + 1}</Text><Button aria-label="上移规则" size="small" disabled={busy || i === 0} icon={<ArrowUpOutlined />} onClick={() => move(i, -1)} /><Button aria-label="下移规则" size="small" disabled={busy || i === config.rules.length - 1} icon={<ArrowDownOutlined />} onClick={() => move(i, 1)} /></Space> },
    { title: '规则', dataIndex: 'name', render: (v, r) => <div><Text strong>{v || '未命名规则'}</Text>{['legacy_codex', 'legacy_dsh'].includes(r.source) && <Tag color="blue" style={{ marginLeft: 6 }}>{r.source === 'legacy_codex' ? 'Codex 迁移' : 'DSH 迁移'}</Tag>}{r.require_end_boundary && <Tag>末尾边界</Tag>}<div className="pg-snippet">{r.match}</div></div> },
    { title: '替换为', dataIndex: 'replacement', render: v => <div className="pg-snippet">{v || <Text type="secondary">删除匹配内容</Text>}</div> },
    { title: '启用', dataIndex: 'enabled', width: 80, render: (v, r) => <Switch aria-label={`启用 ${r.name || '规则'}`} checked={v} disabled={busy} onChange={enabled => saveConfig({ rules: config.rules.map(rule => rule.id === r.id ? { ...rule, enabled } : rule) })} /> },
    { title: '操作', width: 140, render: (_, r) => <Space><Button size="small" icon={<EditOutlined />} disabled={busy} onClick={() => edit(r)}>编辑</Button><Button size="small" danger disabled={busy} onClick={() => modal.confirm({ title: '删除替换规则？', okText: '删除', cancelText: '取消', onOk: () => saveConfig({ rules: config.rules.filter(rule => rule.id !== r.id) }) })}>删除</Button></Space> },
  ];
  const pending = cases.filter(c => !c.resolved).length;
  const active = cases.filter(c => ['running', 'queued'].includes(c.job?.status)).length;
  const detailActive = ['running', 'queued'].includes(detail?.job?.status);
  const candidate = candidateView(detail, config?.revision);
  return <div className="pg-page">
    {error && <Alert type="error" showIcon message={error} action={<Button onClick={load}>重试</Button>} />}
    <div className="pg-stats"><Card><Statistic title="待处理提示词" value={pending} /></Card><Card><Statistic title="已启用替换规则" value={(config?.rules || []).filter(r => r.enabled).length} /></Card><Card><Statistic title="运行或排队中的诊断" value={active} /></Card><Card><Text type="secondary">自动定位</Text><div className="pg-setting-state"><Tag color={config?.settings.auto_locate ? 'blue' : 'default'}>{config?.settings.auto_locate ? '已开启' : '已关闭'}</Tag><Text type="secondary">完整提示词始终记录</Text></div></Card></div>
    <Card title="系统提示词管理" extra={<Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>}>
      <Tabs activeKey={tab} onChange={setTab} items={[
        { key: 'cases', label: `拦截记录${pending ? ` · ${pending} 待处理` : ''}`, children: <>
          <Alert type="info" showIcon message="相同拦截会合并，不会反复自动诊断" description="同一模型、原请求选号范围、原始和出站系统提示词（含角色、内容块和顺序）一致时合并计数，不区分账号。不同分组或动态文本变化会保留为不同记录。" />
          <div className="pg-toolbar"><Text type="secondary">仅记录上游 code=11128；每行是一份去重后的完整提示词记录。</Text><Select aria-label="筛选拦截记录" value={filter} onChange={setFilter} options={[{ value: 'pending', label: '待处理' }, { value: 'all', label: '全部记录' }, { value: 'resolved', label: '已验证通过' }]} /></div>
          <Table rowKey="id" dataSource={cases.filter(c => filter === 'all' || (filter === 'resolved' ? c.resolved : !c.resolved))} pagination={{ pageSize: 10 }} scroll={{ x: 850 }} locale={{ emptyText: '暂无对应的 11128 拦截记录' }} columns={[
            { title: '模型 / 记录', dataIndex: 'model', render: (v, r) => <div><Text strong>{v}</Text><div><Text type="secondary">{r.id.slice(0, 12)}</Text></div></div> },
            { title: '最近拦截', dataIndex: 'last_seen', render: time }, { title: '次数', dataIndex: 'occurrences', width: 70 },
            { title: '处理状态', render: (_, r) => <Space direction="vertical" size={2}><Tag color={r.resolved ? 'green' : 'orange'}>{r.resolved ? '验证通过' : '待处理'}</Tag>{r.job && <Text type="secondary">{r.job.kind === 'verify' ? '替换验证' : '二分定位'} · {r.job.status === 'passed' && !r.resolved ? '历史验证已失效' : states[r.job.status]}<br />已检查 {r.job.calls} 次，最多 {r.job.kind === 'verify' ? 2 : r.job.budget} 次{r.job.cache_hits ? `，另复用 ${r.job.cache_hits} 次` : ''}</Text>}</Space> },
            { title: '操作', render: (_, r) => <Space><Button type="link" onClick={() => setDetailId(r.id)}>查看与处理</Button><Button type="text" danger disabled={['running', 'queued'].includes(r.job?.status)} onClick={() => remove(r)}>删除</Button></Space> },
          ]} />
        </> },
        { key: 'rules', label: '替换规则', children: <>
          {!!config?.legacy_migration_version && <Alert type="success" showIcon message="旧 Codex / DSH 配置已自动迁移" description="规则保留了旧开关的启停状态，可在此修改、关闭或删除。重启不会重新导入，也不会再执行旧兼容替换。" style={{ marginBottom: 12 }} />}
          <Alert type="info" showIcon message="按顺序对每个 system / developer 文本块执行精确、区分大小写的全量替换；后面的规则可以匹配前面的替换结果。" description="支持任意多行内容与空替换，可设置匹配末尾边界，不支持正则。用户消息、工具内容和图片保持原样。" />
          <div className="pg-toolbar"><Text type="secondary">保存后立即生效。修改规则后，旧验证结果需要重新验证。</Text><Button type="primary" icon={<PlusOutlined />} disabled={!config} onClick={() => edit()}>新增规则</Button></div>
          <Table rowKey="id" columns={rulesColumns} dataSource={config?.rules || []} pagination={false} scroll={{ x: 850 }} locale={{ emptyText: '添加任意需要替换的系统提示词内容' }} />
        </> },
        { key: 'settings', label: '诊断设置', children: settings && <Form layout="vertical" className="pg-settings">
          <Alert type="warning" showIcon message="自动定位会发送真实上游请求" description="开启即允许对今后新捕获的 11128 记录自动诊断，单任务按预算串行请求。重复记录不会反复自动重试。关闭会取消自动任务，完整提示词仍会保存，可手动重新定位。" />
          <Form.Item label="自动二分定位新拦截"><Switch aria-label="自动二分定位" checked={settings.auto_locate} onChange={v => setSettings({ ...settings, auto_locate: v })} /></Form.Item>
          <Form.Item label="每次定位最多请求次数" extra="包含对照和完整复现；相同片段在同一任务内复用已确定结果，不重复发送。替换验证单独限制为最多 2 次，对照未通过会提前停止。"><InputNumber aria-label="请求预算" min={4} max={256} value={settings.probe_budget} onChange={v => setSettings({ ...settings, probe_budget: v })} /></Form.Item>
          <Form.Item label="记录保留天数"><InputNumber aria-label="保留天数" min={1} max={365} value={settings.retention_days} onChange={v => setSettings({ ...settings, retention_days: v })} /></Form.Item>
          <Form.Item label="最多保留的记录数" extra="默认 200 条；加密存储总上限 64 MiB，超限会在登录提醒中报告存储错误，提示词不会被截断。"><InputNumber aria-label="记录上限" min={1} max={1000} value={settings.max_cases} onChange={v => setSettings({ ...settings, max_cases: v })} /></Form.Item>
          <Space><Button type="primary" loading={busy} onClick={async () => { const c = await saveConfig({ settings }); if (c) setSettings(c.settings); }}>保存诊断设置</Button><Button onClick={() => setSettings(config.settings)}>撤销未保存设置</Button></Space>
        </Form> },
      ]} />
    </Card>
    <Drawer title="拦截详情与诊断" open={!!detailId} width="min(940px, 100vw)" onClose={() => setDetailId(null)} destroyOnHidden>
      {!detail ? <Empty description="正在加载记录" /> : <Space direction="vertical" size={18} style={{ width: '100%' }}>
        <Descriptions size="small" column={2} items={[{ key: 'model', label: '模型', children: detail.model }, { key: 'status', label: '状态', children: detail.resolved ? '完整提示词已验证通过' : '待处理' }, { key: 'count', label: '拦截次数', children: detail.occurrences }, { key: 'seen', label: '最近拦截', children: time(detail.last_seen) },{key:'scope',label:'原请求范围',children:accountScopeLabel(detail.account_scope)}]} />
        <Space wrap><Button type="primary" disabled={detailActive || active > 0} onClick={() => launch('locate')}>{detail.job ? '重新二分定位' : '开始二分定位'}</Button><Button disabled={detailActive || active > 0} onClick={() => launch('verify')}>验证当前替换</Button><Button onClick={async () => { try { const v = await api(`${root}cases/${detailId}/preview`, { method: 'POST' }); setPreview(v.messages); } catch (e) { message.error(e.message); } }}>本地替换预览</Button>{detailActive && <Button danger onClick={cancel}>取消诊断</Button>}</Space>
        <Alert type="info" showIcon message="验证包含完整系统提示词，但不包含历史用户对话和工具调用；通过只代表本次模型与账号下的这次检测。" />
        {detail.job && <DiagnosticJob item={detail} />}
        {candidate && <Card size="small" className="pg-history" title={candidate.title} extra={<Button onClick={() => edit({ match: detail.candidate.includes('[…]') ? '' : detail.candidate })}>设为替换规则</Button>}><Alert type={candidate.historical ? 'warning' : 'info'} showIcon message={candidate.description} /><div className="pg-job-time"><Text type="secondary">定位时间：{time(detail.candidate_located_at)}</Text><Text type="secondary">配置版本：{detail.candidate_revision || '旧记录未保存'}</Text></div><pre className="pg-candidate">{detail.candidate}</pre><Text type="secondary">若包含 […]，表示不相邻句子的组合，请分别选取原文设置规则。可能还有其他触发片段，需验证完整提示词。</Text></Card>}
        {preview && <PromptText title="按当前规则及兼容处理后的预览（未发送上游）" messages={preview} />}
        <PromptText title="原始完整系统提示词" messages={detail.original} />
        <PromptText title="当时实际出站的完整系统提示词" messages={detail.outbound} />
        {detail.diagnostic && <PromptText title="最近一次定位使用的完整系统提示词" messages={detail.diagnostic} />}
        <Button onClick={() => edit({ match: textOf(detail.original) })}>根据原始提示词新增规则</Button>
      </Space>}
    </Drawer>
    <Modal title={editor?.id ? '编辑替换规则' : '新增替换规则'} open={editor !== null} onCancel={() => setEditor(null)} onOk={saveRule} confirmLoading={busy} width={760} okText="保存规则" cancelText="取消" destroyOnHidden>
      <Form form={ruleForm} layout="vertical"><Form.Item name="name" label="规则名称"><Input maxLength={200} placeholder="例如：路径说明同义改写" /></Form.Item><Form.Item name="match" label="匹配原文（精确匹配）" rules={[{ required: true, message: '请输入要替换的原文' }]}><Input.TextArea rows={6} spellCheck={false} /></Form.Item><Form.Item name="replacement" label="替换内容" extra="留空会删除匹配内容。请自行确认改写前后的语义。"><Input.TextArea rows={6} spellCheck={false} /></Form.Item><Form.Item name="require_end_boundary" label="检查匹配末尾边界" valuePropName="checked" extra="匹配内容之后须为英文句点、空格、制表符、换行或文本结尾；迁移的 DSH 身份规则默认开启，避免误改更长的名称。"><Switch /></Form.Item><Form.Item name="enabled" label="启用规则" valuePropName="checked"><Switch /></Form.Item><Alert type="info" showIcon message="保存规则不会自动验证。保存后可在拦截详情点击“验证当前替换”，确认后发送请求。" /></Form>
    </Modal>
  </div>;
}
