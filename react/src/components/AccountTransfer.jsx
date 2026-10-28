import React, { useEffect, useRef, useState } from 'react';
import { Alert, App, Button, Input, Modal, Space, Steps, Table, Tag, Typography } from 'antd';
import { downloadFile } from '../lib/accountManagement';

const { Text } = Typography;
export default function AccountTransfer({ api, mode, uids = [], onClose, onImported }) {
  const { message } = App.useApp();
  const [password, setPassword] = useState('');
  const [passwordAgain, setPasswordAgain] = useState('');
  const [bundle, setBundle] = useState(null);
  const [fileName, setFileName] = useState('');
  const [preview, setPreview] = useState(null);
  const [selected, setSelected] = useState([]);
  const [result, setResult] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);
  const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const validPassword = new TextEncoder().encode(password).length >= 12 && new TextEncoder().encode(password).length <= 1024;
  const loadFile = async event => {
    const file = event.target.files?.[0]; if (!file) return;
    setPreview(null); setSelected([]); setResult(null); setBundle(null); setError('');
    if (file.size > 16 * 1024 * 1024) { setError('文件超过 16 MiB，请分批导出。'); return; }
    try {
      const content = JSON.parse(await file.text());
      if (content.format !== 'workbuddy2api-accounts' || content.version !== 1 || !content.ciphertext) throw new Error('请选择 WorkBuddy2API 加密账号迁移文件');
      setBundle(content); setFileName(file.name);
    } catch (e) { setError(e instanceof SyntaxError ? '文件不是有效的 JSON 迁移包' : e.message); }
  };
  const act = async () => {
    setBusy(true); setError('');
    try {
      if (mode === 'export') {
        const encrypted = await post('/admin/accounts/export', { uids, password });
        if (!alive.current) return;
        downloadFile(JSON.stringify(encrypted, null, 2), `workbuddy-accounts-${new Date().toISOString().slice(0, 10)}.encrypted.json`);
        message.success(`已生成 ${uids.length} 个账号的加密迁移文件`); onClose();
      } else if (!preview) {
        const body = await post('/admin/accounts/import/preview', { bundle, password });
        if (!alive.current) return;
        setPreview(body.accounts || []); setSelected((body.accounts || []).filter(a => !a.exists && !a.issue).map(a => a.uid));
      } else {
        const body = await post('/admin/accounts/import', { bundle, password, uids: selected });
        if (!alive.current) return;
        setResult(body); setPassword(''); setPasswordAgain(''); setBundle(null); setSelected([]);
        await onImported();
      }
    } catch (e) { if (alive.current) setError(`${e.message}。若执行期间网络中断，请先刷新账号列表核对结果，再重新预览。`); }
    finally { if (alive.current) setBusy(false); }
  };
  return <Modal zIndex={1300} open title={mode === 'export' ? '加密导出账号' : '导入账号'} width={780} onCancel={busy ? undefined : onClose} closable={!busy} maskClosable={!busy} footer={result ? <Button onClick={onClose}>关闭</Button> : <Space wrap><Button disabled={busy} onClick={onClose}>取消</Button>{preview && <Button disabled={busy} onClick={() => { setPreview(null); setSelected([]); }}>返回重新预览</Button>}<Button type="primary" loading={busy} disabled={!validPassword || (mode === 'export' ? password !== passwordAgain || !uids.length : !bundle || (preview && !selected.length))} onClick={act}>{mode === 'export' ? '下载加密文件' : preview ? `确认导入 ${selected.length} 个账号` : '解密并预览'}</Button></Space>}>
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {mode === 'export' ? <><Alert type="info" showIcon message={`导出所选 ${uids.length} 个账号的授权凭据与分组信息，来源账号不会删除或停用。`} description="迁移文件使用密码加密。请妥善保管密码和文件；在目标实例验证后，可自行停用来源账号。" />
        <div><label htmlFor="account-export-password">迁移密码（至少 12 字符）</label><Input.Password id="account-export-password" autoComplete="new-password" value={password} disabled={busy} onChange={e => setPassword(e.target.value)} /></div>
        <div><label htmlFor="account-export-password-again">再次输入密码</label><Input.Password id="account-export-password-again" autoComplete="new-password" value={passwordAgain} disabled={busy} onChange={e => setPasswordAgain(e.target.value)} /></div>
      </> : <>
        <Steps size="small" current={result ? 2 : preview ? 1 : 0} items={[{ title: '文件与密码' }, { title: '核对账号' }, { title: '导入结果' }]} />
        {!result && !preview && <><Alert type="info" showIcon message="选择从另一实例导出的加密 JSON 文件。先预览，不会立即写入账号。" /><label htmlFor="account-import-file">加密迁移文件（最多 16 MiB / 1000 个账号）</label><input id="account-import-file" type="file" accept=".json,application/json" onChange={loadFile} disabled={busy} />{fileName && <Text type="secondary">{fileName}</Text>}<div><label htmlFor="account-import-password">迁移密码</label><Input.Password id="account-import-password" autoComplete="off" value={password} disabled={busy} onChange={e => { setPassword(e.target.value); setPreview(null); }} /></div></>}
        {preview && !result && <><Alert type="warning" showIcon message="新账号导入后均为停用状态，核对后在账号管理页手动启用。" description="重复账号和已有授权文件不覆盖。只保留目标已有且区域兼容的分组，否则尝试 default；临时冷却、代理和统计不迁移。" /><Space wrap><Text>共 {preview.length} 个账号 · 已有 {preview.filter(a => a.exists).length} 个 · 已选择 {selected.length} 个</Text><Button size="small" disabled={busy} onClick={() => setSelected(preview.filter(a => !a.exists && !a.issue).map(a => a.uid))}>选择全部可导入账号</Button></Space>
          <Table className="account-import-table" rowKey="uid" size="small" dataSource={preview} scroll={{ x: 560 }} pagination={{ pageSize: 10, showSizeChanger: false }} rowSelection={{ selectedRowKeys: selected, preserveSelectedRowKeys: true, onChange: setSelected, getCheckboxProps: a => ({ disabled: busy || a.exists || !!a.issue }) }} columns={[{ title: '账号', dataIndex: 'uid', render: (uid, a) => <div><Text>{a.nickname || '未命名'}</Text><div className="account-uid">{uid}</div></div> }, { title: '区域', dataIndex: 'region', render: r => r === 'global' ? '海外版' : '中国区' }, { title: '目标分组', dataIndex: 'groups', render: g => (g || []).join('、') }, { title: '处理方式', render: (_, a) => a.exists ? <Tag>已存在 · 跳过</Tag> : a.issue ? <Text type="danger">{a.issue}</Text> : <Tag color="blue">新增 · 停用</Tag> }]} /></>}
        {result && <><Alert showIcon type={result.success === preview.length ? 'success' : 'info'} message={`成功导入 ${result.success || 0} 个，跳过 ${result.skipped || 0} 个，失败 ${(result.results || []).filter(r => !r.ok && !r.skipped).length} 个`} /><Table rowKey="uid" size="small" dataSource={result.results || []} pagination={{ pageSize: 10, showSizeChanger: false }} columns={[{ title: 'UID', dataIndex: 'uid' }, { title: '结果', render: (_, r) => <Tag color={r.ok ? 'green' : r.skipped ? undefined : 'red'}>{r.ok ? '成功' : r.skipped ? '跳过' : '失败'}</Tag> }, { title: '详情', dataIndex: 'detail' }]} /></>}
      </>}
      {error && <Alert type="error" showIcon message={error} />}
    </Space>
  </Modal>;
}
