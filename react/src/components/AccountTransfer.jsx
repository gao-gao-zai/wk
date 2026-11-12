import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Checkbox, Input, Modal, Progress, Select, Space, Steps, Table, Tag, Typography } from 'antd';
import { downloadFile } from '../lib/accountManagement';
import { accountImportDeviceLabel, accountImportGrowthLabel, accountImportResultType, buildAccountImportRequest, createAccountImportFileLoader, importGrowthJobView, normalizeAccountImportOptions, runMultiAccountImport, validTransferPassword } from '../lib/accountTransfer';

const { Text } = Typography;

function ImportGrowthProgress({ api, jobId }) {
  const [snapshot, setSnapshot] = useState(null);
  const [error, setError] = useState('');
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let disposed = false, timer;
    const controller = new AbortController();
    setSnapshot(null); setError('');
    const read = async () => {
      try {
        const body = await api(`/admin/growth/jobs/${encodeURIComponent(jobId)}`, { signal: controller.signal, cache: 'no-store' });
        if (disposed) return;
        const next = importGrowthJobView(body);
        setSnapshot(next); setError('');
        if (next.active) timer = window.setTimeout(read, 2000);
      } catch {
        if (!disposed) setError('任务进度读取失败，可能已过期或服务重启。可重试查询，导入结果不受影响。');
      }
    };
    void read();
    return () => { disposed = true; window.clearTimeout(timer); controller.abort(); };
  }, [api, jobId, revision]);
  return <Space direction="vertical" size={10} style={{ width: '100%' }}>
    <Space wrap><Text strong>成长任务 {jobId}</Text><Button size="small" onClick={() => setRevision(value => value + 1)}>刷新任务进度</Button></Space>
    <Text type="secondary">只读跟踪当前任务。关闭页面后服务端继续执行；任务结果不改变账号停用状态。</Text>
    {error && <Alert type="warning" showIcon message={error} />}
    {snapshot ? <><Alert type={snapshot.severity} showIcon message={snapshot.label} description={snapshot.error || snapshot.current || '检查任务及奖励状态'} /><Progress percent={snapshot.percent} status={snapshot.severity === 'warning' ? 'exception' : snapshot.active ? 'active' : 'normal'} /><Text>已处理 {snapshot.done} / {snapshot.total || (snapshot.active ? '待确定' : 0)} 项</Text><Table rowKey="key" size="small" pagination={{ pageSize: 10, showSizeChanger: false }} dataSource={snapshot.results} columns={[
      { title: '任务', render: (_, row) => row.desc || row.task_code || '—' },
      { title: '结果', render: (_, row) => <Tag color={!row.ok ? 'red' : row.claimed ? 'green' : undefined}>{!row.ok ? '失败' : row.claimed ? '已领奖' : row.skipped ? '跳过' : '完成'}</Tag> },
      { title: '说明', dataIndex: 'message' },
    ]} /></> : !error && <Text type="secondary">正在读取成长任务进度…</Text>}
  </Space>;
}

export default function AccountTransfer({ api, mode, uids = [], groups = [], groupsError = '', onClose, onImported }) {
  const { message } = App.useApp();
  const [password, setPassword] = useState('');
  const [passwordAgain, setPasswordAgain] = useState('');
  const [source, setSource] = useState(null);
  const [fileName, setFileName] = useState('');
  const [preview, setPreview] = useState(null);
  const [selected, setSelected] = useState([]);
  const [options, setOptions] = useState({ group_mode: 'source', groups: [], nickname: '', run_growth_tasks: false });
  const [previewOptions, setPreviewOptions] = useState(null);
  const [accountCount, setAccountCount] = useState(null);
  const [result, setResult] = useState(null);
  const [busy, setBusy] = useState(false);
  const [loadingFile, setLoadingFile] = useState(false);
  const [error, setError] = useState('');
  const [growthJobID, setGrowthJobID] = useState('');
  const [progress, setProgress] = useState(null);
  const [stopping, setStopping] = useState(false);
  const stopRequested = useRef(false);
  const alive = useRef(true);
  const fileLoader = useRef(null);
  if (!fileLoader.current) fileLoader.current = createAccountImportFileLoader();
  const acting = useRef(false);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; fileLoader.current.invalidate(); };
  }, []);
  const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const encrypted = source?.kind === 'encrypted';
  const validPassword = validTransferPassword(password);
  const singleAccount = source?.kind === 'credential' || accountCount === 1;
  let optionError = '';
  try { normalizeAccountImportOptions(options, singleAccount); } catch (e) { optionError = e.message; }
  const changeOptions = patch => {
    setOptions(current => ({ ...current, ...patch }));
    setPreview(null); setPreviewOptions(null); setSelected([]); setError('');
  };
  const loadFile = async event => {
    const files = Array.from(event.target.files || []);
    // Allow choosing the same files again after a read error.
    event.target.value = '';
    if (!files.length) return;
    setPreview(null); setPreviewOptions(null); setSelected([]); setResult(null); setSource(null); setError('');
    setOptions({ group_mode: 'source', groups: [], nickname: '', run_growth_tasks: false }); setAccountCount(null); setGrowthJobID('');
    setPassword(''); setPasswordAgain(''); setFileName(''); setLoadingFile(true);
    const loaded = await fileLoader.current.loadMany(files);
    if (!alive.current || !loaded) return;
    setSource(loaded.source); setFileName(loaded.fileName); setError(loaded.error); setLoadingFile(false);
  };
  const act = async () => {
    if (acting.current || loadingFile) return;
    acting.current = true;
    stopRequested.current = false;
    setStopping(false); setProgress(null);
    setBusy(true); setError('');
    const cancelled = () => !alive.current || stopRequested.current;
    const onProgress = update => { if (alive.current) setProgress(update); };
    try {
      if (mode === 'export') {
        const encrypted = await post('/admin/accounts/export', { uids, password });
        if (!alive.current) return;
        downloadFile(JSON.stringify(encrypted, null, 2), `workbuddy-accounts-${new Date().toISOString().slice(0, 10)}.encrypted.json`);
        message.success(`已生成 ${uids.length} 个账号的加密迁移文件`); onClose();
      } else if (!preview) {
        const submittedOptions = normalizeAccountImportOptions(options, singleAccount);
        const body = source.kind === 'credentials'
          ? await runMultiAccountImport(post, source.sources, submittedOptions, undefined, cancelled, onProgress)
          : await post('/admin/accounts/import/preview', buildAccountImportRequest(source, password, undefined, submittedOptions, singleAccount));
        if (!alive.current) return;
        if (body.stop) { setError('预览已停止，请重新预览全部账号。'); return; }
        setPreviewOptions(submittedOptions); setAccountCount(body.count ?? (body.accounts || []).length);
        setPreview(body.accounts || []); setSelected((body.accounts || []).filter(a => !a.exists && !a.issue).map(a => a.uid));
      } else {
        const body = source.kind === 'credentials'
          ? await runMultiAccountImport(post, source.sources, previewOptions, selected, cancelled, onProgress)
          : await post('/admin/accounts/import', buildAccountImportRequest(source, password, selected, previewOptions, singleAccount));
        if (!alive.current) return;
        setResult(body); setPassword(''); setPasswordAgain(''); setSource(null); setSelected([]);
        const jobs = (body.results || []).filter(row => row.growth_job_id);
        setGrowthJobID((jobs.find(row => row.growth_status === 'queued') || jobs[0])?.growth_job_id || '');
        await onImported();
      }
    } catch (e) { if (alive.current) setError(`${e.message}。若执行期间网络中断，请先刷新账号列表核对结果，再重新预览。`); }
    finally { acting.current = false; if (alive.current) { setBusy(false); setProgress(null); setStopping(false); } }
  };
  return <Modal zIndex={1300} open title={mode === 'export' ? '加密导出账号' : '导入账号'} width={780} onCancel={busy ? undefined : onClose} closable={!busy} maskClosable={!busy} footer={result ? <Button onClick={onClose}>关闭</Button> : <Space wrap>
    {busy && source?.kind === 'credentials' ? <Button disabled={stopping} onClick={() => { stopRequested.current = true; setStopping(true); }}> {stopping ? '正在停止' : '停止后续操作'} </Button> : <Button disabled={busy} onClick={onClose}>取消</Button>}
    {preview && <Button disabled={busy} onClick={() => { setPreview(null); setPreviewOptions(null); setSelected([]); }}>返回重新预览</Button>}
    <Button type="primary" loading={busy || loadingFile} disabled={loadingFile || (mode === 'export' ? !validPassword || password !== passwordAgain || !uids.length : !source || !!optionError || (encrypted && !validPassword) || (preview && !selected.length))} onClick={act}>{mode === 'export' ? '下载加密文件' : preview ? `确认导入 ${selected.length} 个账号` : encrypted ? '解密并预览' : '预览账号'}</Button>
  </Space>}>
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {mode === 'export' ? <><Alert type="info" showIcon message={`导出所选 ${uids.length} 个账号的授权凭据与分组信息，来源账号不会删除或停用。`} description="迁移文件使用密码加密。请妥善保管密码和文件；在目标实例验证后，可自行停用来源账号。" />
        <div><label htmlFor="account-export-password">迁移密码（至少 12 字符）</label><Input.Password id="account-export-password" autoComplete="new-password" value={password} disabled={busy} onChange={e => setPassword(e.target.value)} /></div>
        <div><label htmlFor="account-export-password-again">再次输入密码</label><Input.Password id="account-export-password-again" autoComplete="new-password" value={passwordAgain} disabled={busy} onChange={e => setPasswordAgain(e.target.value)} /></div>
      </> : <>
        <Steps size="small" current={result ? 2 : preview ? 1 : 0} items={[{ title: '选择账号文件' }, { title: '核对账号' }, { title: '导入结果' }]} />
        {busy && progress && <div className="account-transfer-progress" role="status" aria-live="polite">
          <div className="account-transfer-progress-header"><div><strong>{preview ? '正在导入账号' : '正在预览账号'}</strong><div className="account-transfer-progress-caption">{stopping ? '正在等待当前请求完成，后续账号不会提交。' : '逐个处理，请保持页面打开；停止仅阻止后续导入，不取消已排队成长任务。'}</div></div><span className="account-transfer-progress-count">{progress.completed} <span>/ {progress.total}</span></span></div>
          <Progress percent={progress.total ? Math.floor(progress.completed * 100 / progress.total) : 0} showInfo={false} status="active" strokeColor={{ '0%': '#5b8def', '100%': '#6558d3' }} />
          <div className="account-transfer-progress-footer"><span>已处理 {progress.completed} 个</span><span>剩余 {progress.total - progress.completed} 个</span></div>
        </div>}
        {!result && !preview && <><Alert type="info" showIcon message="支持选择多个官方 auth/account 凭据 JSON，或单独选择一个 WorkBuddy2API 加密迁移包。" description="原始凭据文件无需密码。先预览，再选择导入；凭据只用于本次导入，不会在页面显示。" /><label htmlFor="account-import-file">账号文件（官方凭据最多 64 KiB；加密包最多 16 MiB / 1000 个账号）</label><input id="account-import-file" type="file" multiple onChange={loadFile} disabled={busy} />{fileName && <Text type="secondary">{fileName}</Text>}{source?.kind === 'credential' && <Text type="secondary">已识别为单个官方凭据文件，无需迁移密码。</Text>}{encrypted && <div><label htmlFor="account-import-password">迁移密码</label><Input.Password id="account-import-password" autoComplete="off" value={password} disabled={busy} onChange={e => { setPassword(e.target.value); setPreview(null); }} /></div>}</>}
        {source && !result && <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Text strong>导入参数</Text>
          <label htmlFor="account-import-group-mode">分组策略</label>
          <Select id="account-import-group-mode" style={{ width: '100%' }} value={options.group_mode} disabled={busy} onChange={group_mode => changeOptions({ group_mode })} options={[
            { value: 'source', label: '沿用来源兼容分组（官方凭据使用 default）' },
            { value: 'replace', label: '替换为指定分组' },
            { value: 'append', label: '在来源兼容分组基础上追加' },
          ]} />
          {groupsError && <Alert type="warning" showIcon message={groupsError} />}
          {options.group_mode !== 'source' && <><label htmlFor="account-import-groups">目标分组</label><Select id="account-import-groups" mode="multiple" style={{ width: '100%' }} placeholder="至少选择一个目标分组" value={options.groups} disabled={busy} onChange={groups => changeOptions({ groups })} options={groups.map(group => ({ value: group.name, label: `${group.name} · ${group.region === 'cn' ? '中国区' : group.region === 'global' ? '海外版' : '所有区域'}` }))} /><Text type="secondary">目标分组必须存在且区域兼容，预览时会逐项校验。</Text></>}
          {singleAccount ? <><label htmlFor="account-import-nickname">自定义昵称（留空保留原昵称）</label><Input id="account-import-nickname" value={options.nickname} disabled={busy} placeholder="最多 128 个字符" showCount={{ formatter: ({ value }) => `${Array.from(value || '').length}/128` }} onChange={event => changeOptions({ nickname: event.target.value })} /></> : <Text type="secondary">{encrypted && accountCount === null ? '加密包预览确认只有一个账号后，可自定义昵称。' : '批量导入保留每个账号原昵称。'}</Text>}
          <Checkbox checked={options.run_growth_tasks} disabled={busy} onChange={event => changeOptions({ run_growth_tasks: event.target.checked })}>导入后自动检查并完成未完成成长任务（默认关闭）</Checkbox>
          {options.run_growth_tasks && <Alert type="warning" showIcon message="将产生真实上游任务请求及必要的短对话，可能消耗用量。" description="仅为本次确定成功新增的中国区账号排队；重复、失败、待核对及海外版账号不执行。新账号始终保持停用，任务失败不回滚成功导入。" />}
          <Text type="secondary">缺少设备档案时将为新账号生成 Mac 设备档案；已有设备档案保持原值。更改导入参数后需重新预览。</Text>
          {optionError && <Alert type="error" showIcon message={optionError} />}
        </Space>}
        {preview && !result && <><Alert type="warning" showIcon message="新账号导入后均为停用状态，核对后在账号管理页手动启用。" description="重复账号和已有授权文件不覆盖。请核对目标分组与设备处理方式；临时冷却、代理和统计不迁移。" /><Space wrap><Text>共 {preview.length} 个账号 · 已有 {preview.filter(a => a.exists).length} 个 · 已选择 {selected.length} 个</Text><Button size="small" disabled={busy} onClick={() => setSelected(preview.filter(a => !a.exists && !a.issue).map(a => a.uid))}>选择全部可导入账号</Button></Space>
          <Table className="account-import-table" rowKey="uid" size="small" dataSource={preview} scroll={{ x: 920 }} pagination={{ pageSize: 10, showSizeChanger: false }} rowSelection={{ selectedRowKeys: selected, preserveSelectedRowKeys: true, onChange: setSelected, getCheckboxProps: a => ({ disabled: busy || a.exists || !!a.issue }) }} columns={[{ title: '账号', dataIndex: 'uid', render: (uid, a) => <div><Text>{a.nickname || '未命名'}</Text><div className="account-uid">{uid}</div></div> }, { title: '区域', dataIndex: 'region', render: r => r === 'global' ? '海外版' : '中国区' }, { title: '目标分组', dataIndex: 'groups', render: g => (g || []).join('、') }, { title: '设备', key: 'device', render: (_, account) => accountImportDeviceLabel(account) }, { title: '成长任务', key: 'growth', render: (_, account) => accountImportGrowthLabel(account, previewOptions?.run_growth_tasks) }, { title: '处理方式', render: (_, a) => a.exists ? <Tag>已存在 · 跳过</Tag> : a.issue ? <Text type="danger">{a.issue}</Text> : <Tag color="blue">新增 · 停用</Tag> }]} /></>}
        {result && <><Alert showIcon type={accountImportResultType(result)} message={`成功导入 ${result.success || 0} 个，跳过 ${result.skipped || 0} 个，未提交 ${(result.results || []).filter(r => r.cancelled).length} 个，失败 ${(result.results || []).filter(r => !r.ok && !r.skipped && !r.cancelled).length} 个`} /><Table rowKey="uid" size="small" dataSource={result.results || []} pagination={{ pageSize: 10, showSizeChanger: false }} columns={[{ title: 'UID', dataIndex: 'uid' }, { title: '结果', render: (_, r) => <Tag color={r.ok ? 'green' : r.skipped || r.cancelled ? undefined : 'red'}>{r.ok ? '成功' : r.skipped ? '跳过' : r.cancelled ? '未提交' : '失败'}</Tag> }, { title: '详情', dataIndex: 'detail' }, { title: '成长任务', render: (_, row) => <Space direction="vertical" size={3}><Text>{accountImportGrowthLabel(row, previewOptions?.run_growth_tasks)}</Text>{row.growth_detail && <Text type="secondary">{row.growth_detail}</Text>}{row.growth_job_id && <Button type="link" size="small" onClick={() => setGrowthJobID(row.growth_job_id)}>查看进度</Button>}</Space> }]} />{growthJobID && <ImportGrowthProgress key={growthJobID} api={api} jobId={growthJobID} />}<Text type="secondary">查看其它任务可进入<a href="#/growth" onClick={onClose}>成长任务中心</a>；关闭此窗口不会取消服务端任务。</Text></>}
      </>}
      {error && <Alert type="error" showIcon message={error} />}
    </Space>
  </Modal>;
}
