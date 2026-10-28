import React, { useEffect, useMemo, useRef, useState } from 'react';
import { AccountSnapshots, CollectionLaunch, CollectionTag } from '../components/Collection';
import { Alert, App, Button, Card, Checkbox, Descriptions, Drawer, Dropdown, Empty, Input, Modal, Progress, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import { ApartmentOutlined, DownloadOutlined, MoreOutlined, PlusOutlined, ReloadOutlined, SearchOutlined, SettingOutlined, UploadOutlined } from '@ant-design/icons';
import { accountCSV, accountReason, accountState, deadline, downloadFile, filterAccounts, groupChange, groupsOf, runAccountBatch, stateColors, stateLabels } from '../lib/accountManagement';
import AccountTransfer from '../components/AccountTransfer';

const { Text } = Typography;
const EMPTY = {};
const initialFilters = { search: '', status: 'all', region: 'all', group: 'all', expiry: 'all' };
const labels = { enable: '启用', disable: '禁用', 'clear-cooldown': '清除冷却', checkin: '签到', keepalive: '保活', delete: '删除', groups: '调整分组' };
const fmt = value => Number(value || 0).toLocaleString();
const time = value => deadline(value) ? new Date(value).toLocaleString() : '—';
function expiry(record) {
  const expires = Number(record.token_expires_at || 0) * 1000;
  const remaining = expires - Date.now();
  return expires ? <Tooltip title={new Date(expires).toLocaleString()}><Text type={remaining <= 0 ? 'danger' : remaining < 86400000 ? 'warning' : 'secondary'}>{remaining <= 0 ? '已过期' : remaining < 86400000 ? `${Math.ceil(remaining / 3600000)} 小时后到期` : `${Math.floor(remaining / 86400000)} 天后到期`}</Text></Tooltip> : <Text type="secondary">未知</Text>;
}
function Cooldown({ record }) {
  const until = Math.max(deadline(record.until), deadline(record.breaker_until), deadline(record.auth_until));
  const [now, setNow] = useState(Date.now());
  useEffect(() => { if (until <= Date.now()) return undefined; const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, [until]);
  const seconds = Math.max(0, Math.ceil((until - now) / 1000));
  return seconds > 0 ? <Text type="secondary">{Math.floor(seconds / 60)} 分 {seconds % 60} 秒后重试</Text> : null;
}

export default function AccountList({ api, data, refresh, refreshCredits, creditRefreshing }) {
  const { message, modal } = App.useApp();
  const accounts = data.accounts || [];
  const mapping = data.account_groups || EMPTY;
  const [filters, setFilters] = useState(initialFilters);
  const [selected, setSelected] = useState([]);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(25);
  const [groups, setGroups] = useState([]);
  const [groupsError, setGroupsError] = useState('');
  const [detailUID, setDetailUID] = useState(null);
  const [columnsShown, setColumnsShown] = useState(['groups', 'credits', 'expiry', 'flight']);
  const [confirm, setConfirm] = useState(null);
  const [deleteText, setDeleteText] = useState('');
  const [groupMode, setGroupMode] = useState('replace');
  const [targetGroups, setTargetGroups] = useState([]);
  const [job, setJob] = useState(null);
  const [jobOpen, setJobOpen] = useState(false);
  const [transfer, setTransfer] = useState(null);
  const [refreshing, setRefreshing] = useState(false);
  const alive = useRef(true);
  const active = useRef(null);
  useEffect(() => { alive.current = true; return () => { alive.current = false; if (active.current) active.current.cancelled = true; }; }, []);
  useEffect(() => {
    let disposed = false;
    api('/admin/groups').then(body => { if (!disposed) { setGroups(body.groups || []); setGroupsError(''); } }).catch(() => { if (!disposed) setGroupsError('分组加载失败，可刷新列表重试。'); });
    return () => { disposed = true; };
  }, [api, data.account_groups]);
  useEffect(() => { const live = new Set(accounts.map(a => a.uid)); setSelected(keys => keys.every(uid => live.has(uid)) ? keys : keys.filter(uid => live.has(uid))); }, [data.accounts]);
  const filtered = useMemo(() => filterAccounts(accounts, mapping, filters), [data.accounts, mapping, filters]);
  const byUID = useMemo(() => new Map(accounts.map(a => [a.uid, a])), [data.accounts]);
  const detail = byUID.get(detailUID);
  const running = !!job?.running;
  const summary = useMemo(() => { const result = { healthy: 0, cooling: 0, disabled: 0, model_cooling: 0, credits: 0 }; accounts.forEach(a => { result[accountState(a)]++; result.credits += Number(a.credits || 0); }); return result; }, [data.accounts]);
  const setFilter = (key, value) => { setFilters(current => ({ ...current, [key]: value })); setSelected([]); setPage(1); };
  const reset = () => { setFilters(initialFilters); setSelected([]); setPage(1); };
  const reload = async () => { setRefreshing(true); try { await refresh(); } catch (e) { message.error(e.message); } finally { if (alive.current) setRefreshing(false); } };
  const ask = (action, uids = selected) => { if (!uids.length || active.current) return; setDeleteText(''); setTargetGroups([]); setGroupMode('replace'); setConfirm({ action, uids: [...uids] }); };
  const execute = async operation => {
    if (active.current) return;
    const token = { cancelled: false }; active.current = token; setConfirm(null); setJobOpen(true);
    setJob({ operation, running: true, rows: [], total: operation.uids.length });
    try {
      await runAccountBatch(operation.uids, async uid => {
        if (operation.action === 'groups') {
          const current = await api(`/admin/accounts/${encodeURIComponent(uid)}/groups`);
          if (token.cancelled) return { ok: false, detail: '已取消，未修改分组' };
          const next = groupChange(current.groups?.length ? current.groups : ['default'], operation.groups, operation.mode);
          return api(`/admin/accounts/${encodeURIComponent(uid)}/groups`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ groups: next }) });
        }
        return api(`/admin/account/${encodeURIComponent(uid)}${operation.action === 'delete' ? '' : `/${operation.action}`}`, { method: operation.action === 'delete' ? 'DELETE' : 'POST' });
      }, { cancelled: () => token.cancelled, onResult: row => { if (alive.current) setJob(current => ({ ...current, rows: [...current.rows, row] })); } });
    } finally { active.current = null; if (alive.current) { setJob(current => ({ ...current, running: false })); setSelected([]); await reload(); } }
  };
  const count = status => job?.rows.filter(r => r.status === status).length || 0;
  const compatibleGroups = groups.filter(g => groupMode === 'remove' || confirm?.uids.every(uid => g.region === 'all' || !g.region || g.region === (byUID.get(uid)?.region || 'cn')));
  const moreItems = [
    { key: 'clear-cooldown', label: '清除冷却 / 熔断' }, { key: 'groups', label: '设置分组', disabled: !groups.length }, { type: 'divider' },
    { key: 'checkin', label: '签到（请求上游）' }, { key: 'keepalive', label: '保活（请求上游）' }, { type: 'divider' }, { key: 'delete', label: '删除账号', danger: true },
  ];
  const columns = [
    { title: '账号', key: 'account', width: 210, render: (_, a) => <div className="account-identity"><Button type="link" onClick={() => setDetailUID(a.uid)}>{a.nickname || '未命名账号'}</Button><Text type="secondary" className="account-uid" title={a.uid}>{a.uid}</Text></div> },
    { title: '区域', dataIndex: 'region', width: 90, render: value => <Tag color={value === 'global' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : '中国区'}</Tag> },
    { title: '分组', key: 'groups', width: 140, render: (_, a) => <Space size={2} wrap>{groupsOf(mapping, a.uid).map(g => <Tag key={g}>{g}</Tag>)}</Space> },
    { title: '状态 / 原因', key: 'status', width: 220, render: (_, a) => <Space direction="vertical" size={3}><Tag color={stateColors[accountState(a)]}>{stateLabels[accountState(a)]}</Tag>{a.reason && <Text className="account-reason" title={accountReason(a.reason)}>{accountReason(a.reason)}</Text>}<Cooldown record={a} /></Space> },
    { title: '上游积分快照', key: 'credits', dataIndex: 'credits', width: 190, sorter: (a, b) => (a.credits || 0) - (b.credits || 0), render: (v, a) => <Space direction="vertical" size={2}><strong>{a.credit_updated_at ? fmt(v) : '未知'}</strong><Text type="secondary" style={{ fontSize: 12 }}>{a.credit_updated_at ? new Date(a.credit_updated_at * 1000).toLocaleString() : '尚未成功采集'}</Text>{data.collection_balance?.[a.uid]?.error && <Tooltip title={data.collection_balance[a.uid].error}><CollectionTag status={data.collection_balance[a.uid].state} /></Tooltip>}{data.collection_balance?.[a.uid]?.stale && a.credit_updated_at > 0 && <Text type="warning">快照已过期</Text>}{a.estimated_spent > 0 && <Tooltip title="根据最近上游快照扣除本地请求消耗估算，供路由参考"><Text type="secondary">估算可用 {fmt(a.effective_credits)}</Text></Tooltip>}</Space> },
    { title: '凭证有效期', key: 'expiry', width: 140, sorter: (a, b) => (a.token_expires_at || 0) - (b.token_expires_at || 0), render: (_, a) => expiry(a) },
    { title: '成功率', key: 'success', width: 90, render: (_, a) => a.success_count + a.err_total > 0 ? `${Math.round(a.success_count * 100 / (a.success_count + a.err_total))}%` : '—' },
    { title: '在途', key: 'flight', dataIndex: 'in_flight', width: 70, render: v => v || 0 },
    { title: '操作', key: 'actions', width: 170, fixed: 'right', render: (_, a) => <Space size={2}><Button size="small" type="link" disabled={running} onClick={() => setDetailUID(a.uid)}>详情</Button><Button size="small" type="link" disabled={running} onClick={() => ask(a.disabled ? 'enable' : 'disable', [a.uid])}>{a.disabled ? '启用' : '禁用'}</Button><Dropdown trigger={['click']} disabled={running} menu={{ items: moreItems, onClick: ({ key }) => ask(key, [a.uid]) }}><Button size="small" type="text" aria-label={`更多操作 ${a.uid}`} icon={<MoreOutlined />} /></Dropdown></Space> },
  ].filter(c => !['groups', 'credits', 'expiry', 'flight', 'success'].includes(c.key) || columnsShown.includes(c.key));
  const toolMenu = { items: [{ key: 'csv', label: '导出当前筛选清单（不含凭据）' }], onClick: ({ key }) => {
    if (key === 'csv') downloadFile(accountCSV(filtered, mapping), 'account-inventory.csv', 'text/csv;charset=utf-8');

  } };
  return <div className="account-manager">
    <div className="account-toolbar"><div><Text type="secondary">统一管理中国区与海外版账号</Text><div className="account-toolbar-hint">筛选、选择账号后，可批量操作或迁移。</div></div><Space wrap><Button icon={<PlusOutlined />} onClick={() => { location.hash = '#/accounts'; }} disabled={running}>添加账号</Button><Button icon={<UploadOutlined />} onClick={() => setTransfer({ mode: 'import' })} disabled={running}>导入账号</Button><Button icon={<ReloadOutlined />} onClick={reload} loading={refreshing} disabled={running}>刷新列表</Button><CollectionLaunch api={api} uids={filtered.map(a => a.uid)} title="当前筛选结果" disabled={!filtered.length} onComplete={reload}>采集筛选结果</CollectionLaunch><CollectionLaunch api={api} onComplete={reload}>采集全池</CollectionLaunch></Space></div>
    <div className="account-summary">{[['all', '全部账号', accounts.length], ['healthy', '可用', summary.healthy], ['cooling', '冷却 / 熔断', summary.cooling], ['disabled', '已禁用', summary.disabled]].map(([status, title, value]) => <button key={status} className={`account-summary-card ${filters.status === status ? 'active' : ''}`} disabled={running} onClick={() => setFilter('status', status)}><span>{title}</span><strong>{fmt(value)}</strong></button>)}<div className="account-summary-balance"><span>全池剩余积分</span><strong>{fmt(summary.credits)}</strong></div></div>
    <Card className="account-filter-card"><div className="account-filters">
      <Input aria-label="搜索账号" prefix={<SearchOutlined />} placeholder="搜索昵称、UID 或冷却原因" allowClear value={filters.search} disabled={running} onChange={e => setFilter('search', e.target.value)} />
      <Select aria-label="账号状态筛选" value={filters.status} disabled={running} onChange={v => setFilter('status', v)} options={[{ value: 'all', label: '全部状态' }, ...Object.entries(stateLabels).map(([value, label]) => ({ value, label }))]} />
      <Select aria-label="区域筛选" value={filters.region} disabled={running} onChange={v => setFilter('region', v)} options={[{ value: 'all', label: '全部区域' }, { value: 'cn', label: '中国区' }, { value: 'global', label: '海外版' }]} />
      <Select aria-label="分组筛选" value={filters.group} disabled={running} onChange={v => setFilter('group', v)} options={[{ value: 'all', label: '全部分组' }, ...groups.map(g => ({ value: g.name, label: g.name }))]} />
      <Select aria-label="凭证筛选" value={filters.expiry} disabled={running} onChange={v => setFilter('expiry', v)} options={[{ value: 'all', label: '全部凭证' }, { value: 'expired', label: '已过期' }, { value: 'soon', label: '24 小时内到期' }, { value: 'unknown', label: '到期时间未知' }]} />
      <Button onClick={reset} disabled={running}>重置</Button>
    </div></Card>
    {groupsError && <Alert type="warning" message={groupsError} showIcon />}
    {selected.length > 0 && <div className="account-batch-bar"><Space wrap><strong>已选择 {selected.length} 个账号</strong><CollectionLaunch api={api} uids={selected} title="已选账号" onComplete={reload}>采集已选</CollectionLaunch><Button type="link" disabled={running} onClick={() => setSelected([])}>清空选择</Button><Button disabled={running || filtered.length > 1000} onClick={() => setSelected(filtered.map(a => a.uid))}>选择全部筛选结果（{filtered.length}）</Button></Space><Space wrap>
      {['enable', 'disable', 'clear-cooldown'].map(action => <Button key={action} onClick={() => ask(action)} disabled={running}>{labels[action]}</Button>)}<Button icon={<ApartmentOutlined />} onClick={() => ask('groups')} disabled={running || !groups.length}>分组迁移</Button><Button icon={<DownloadOutlined />} disabled={running} onClick={() => setTransfer({ mode: 'export', uids: [...selected] })}>加密导出</Button><Dropdown trigger={['click']} disabled={running} menu={{ items: [{ key: 'checkin', label: '批量签到（请求上游）' }, { key: 'keepalive', label: '批量保活（请求上游）' }, { key: 'delete', label: '批量删除', danger: true }], onClick: ({ key }) => ask(key) }}><Button>更多 <MoreOutlined /></Button></Dropdown>
    </Space></div>}
    {job && <Alert type={running ? 'info' : count('failed') ? 'warning' : 'success'} showIcon message={<Space wrap><span>{labels[job.operation.action]}：已处理 {job.rows.length} / {job.total}，成功 {count('success')}，失败 {count('failed')}，未执行 {count('cancelled')}</span><Button size="small" onClick={() => setJobOpen(true)}>查看结果</Button>{running && <Button size="small" onClick={() => { if (active.current) active.current.cancelled = true; }}>取消后续操作</Button>}</Space>} />}
    <Card className="account-table-card" title={<Space wrap><span>账号</span><Text type="secondary" style={{ fontSize: 13, fontWeight: 400 }}>筛选结果 {filtered.length} / {accounts.length}</Text></Space>} extra={<Space wrap><Dropdown trigger={['click']} popupRender={() => <Card size="small"><Checkbox.Group value={columnsShown} onChange={setColumnsShown} options={[{ value: 'groups', label: '分组' }, { value: 'credits', label: '可用积分' }, { value: 'expiry', label: '凭证有效期' }, { value: 'flight', label: '在途' }, { value: 'success', label: '成功率' }]} /></Card>}><Button size="small" icon={<SettingOutlined />}>显示列</Button></Dropdown><Dropdown trigger={['click']} menu={toolMenu}><Button size="small" icon={<MoreOutlined />} aria-label="账号表工具" /></Dropdown></Space>}>
      <Table rowKey="uid" columns={columns} dataSource={filtered} size="middle" scroll={{ x: Math.max(1000, columns.reduce((sum, c) => sum + c.width, 48)) }} rowSelection={{ selectedRowKeys: selected, preserveSelectedRowKeys: true, onChange: keys => setSelected(keys.slice(0, 1000)), getCheckboxProps: () => ({ disabled: running }), columnWidth: 44 }} pagination={{ current: page, pageSize, showSizeChanger: true, pageSizeOptions: [25, 50, 100], showTotal: total => `共 ${total} 个账号`, onChange: (next, size) => { setPage(size === pageSize ? next : 1); setPageSize(size); } }} locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={accounts.length ? '没有匹配的账号，试试重置筛选' : '暂无账号，可添加或导入账号'} /> }} />
    </Card>
    <Modal zIndex={1300} cancelText="取消" title={`${labels[confirm?.action] || ''}账号`} open={!!confirm} onCancel={() => setConfirm(null)} okText={confirm?.action === 'delete' ? '确认删除' : '确认执行'} okButtonProps={{ danger: confirm?.action === 'delete', disabled: confirm?.action === 'delete' ? deleteText !== 'DELETE' : confirm?.action === 'groups' && !targetGroups.length }} onOk={() => { void execute({ ...confirm, groups: [...targetGroups], mode: groupMode }); }}>
      <p>将对已选定的 <strong>{confirm?.uids.length || 0}</strong> 个账号执行{labels[confirm?.action]}。每项独立处理，失败项会保留原因。</p>
      {confirm?.action === 'delete' && <><Alert type="error" showIcon message="删除账号及授权文件，无法直接恢复。有在途请求的账号会报错并保留。" /><Input aria-label="删除确认" placeholder="输入 DELETE 确认删除" value={deleteText} onChange={e => setDeleteText(e.target.value)} style={{ marginTop: 12 }} /></>}
      {confirm?.action === 'enable' && <Alert type="warning" message="启用将清除禁用及冷却状态；上游仍拒绝的账号可能再次被禁用。" />}
      {confirm?.action === 'disable' && <Alert type="info" message="停止接收新请求，正在处理的请求继续完成。" />}
      {confirm?.action === 'clear-cooldown' && <Alert type="info" message="清除账号/模型冷却及熔断，保留禁用状态。" />}
      {['checkin', 'keepalive'].includes(confirm?.action) && <Alert type="warning" showIcon message="将串行发送真实上游请求，可能产生用量。取消只停止尚未发出的请求。" />}
      {confirm?.action === 'groups' && <Space direction="vertical" style={{ width: '100%' }}><Select aria-label="分组操作方式" style={{ width: '100%' }} value={groupMode} onChange={v => { setGroupMode(v); setTargetGroups([]); }} options={[{ value: 'replace', label: '迁移到目标分组（替换现有归属）' }, { value: 'append', label: '追加分组（保留现有归属）' }, { value: 'remove', label: '移出指定分组' }]} /><Select aria-label="目标分组" mode="multiple" placeholder="选择目标分组" style={{ width: '100%' }} value={targetGroups} onChange={setTargetGroups} options={compatibleGroups.map(g => ({ value: g.name, label: `${g.name} · ${g.region === 'all' ? '所有区域' : g.region === 'global' ? '海外版' : '中国区'}` }))} /><Text type="secondary">追加/移出前读取最新归属。移出最后一个分组后回到 default。只列出区域兼容的目标。</Text></Space>}
      <div className="account-target-preview">{confirm?.uids.slice(0, 8).map(uid => <div key={uid}>{byUID.get(uid)?.nickname || '未命名'} · {uid}</div>)}{confirm?.uids.length > 8 && <div>以及另外 {confirm.uids.length - 8} 个账号</div>}</div>
    </Modal>
    <Drawer title="账号详情" open={!!detailUID} onClose={() => setDetailUID(null)} width={600} rootClassName="account-drawer">
      {detail ? <><Descriptions column={1} bordered size="small" items={[
        ['昵称', detail.nickname || '未命名'], ['UID', <Text copyable>{detail.uid}</Text>], ['区域', detail.region === 'global' ? '海外版' : '中国区'], ['状态', stateLabels[accountState(detail)]], ['分组', groupsOf(mapping, detail.uid).join('、')], ['原因', detail.reason || '—'], ['剩余积分', fmt(detail.credits)], ['周期积分', `${fmt(detail.cycle_capacity_used)} / ${fmt(detail.cycle_capacity_size)}`], ['累计积分', `${fmt(detail.capacity_used)} / ${fmt(detail.capacity_size)}`], ['积分更新时间', detail.credit_updated_at ? new Date(detail.credit_updated_at * 1000).toLocaleString() : '—'], ['凭证到期', detail.token_expires_at ? new Date(detail.token_expires_at * 1000).toLocaleString() : '未知'], ['冷却截止', time(detail.until)], ['熔断截止', time(detail.breaker_until)], ['鉴权恢复截止', time(detail.auth_until)], ['模型限流', Object.entries(detail.model_cooldowns || {}).map(([model, limit]) => `${model}：${limit.reason || '限流'}；${time(limit.until)}`).join('\n') || '—'], ['成功 / 错误', `${fmt(detail.success_count)} / ${fmt(detail.err_total)}`], ['在途请求', detail.in_flight || 0], ['最近成功', time(detail.last_success)], ['最近错误', time(detail.last_err)],
      ].map(([label, children]) => ({ key: label, label, children }))} /><AccountSnapshots api={api} uid={detail.uid} /><Space wrap style={{ marginTop: 20 }}><CollectionLaunch api={api} uids={[detail.uid]} title={detail.nickname || detail.uid} onComplete={reload} /><Button disabled={running} onClick={() => ask(detail.disabled ? 'enable' : 'disable', [detail.uid])}>{detail.disabled ? '启用账号' : '禁用账号'}</Button><Button disabled={running} onClick={() => setTransfer({ mode: 'export', uids: [detail.uid] })}>加密导出</Button><Button danger disabled={running} onClick={() => ask('delete', [detail.uid])}>删除</Button></Space></> : <Empty description="账号已不在当前列表中" />}
    </Drawer>
    <Drawer title={`${labels[job?.operation.action] || ''}操作结果`} open={jobOpen} onClose={() => setJobOpen(false)} width={720} rootClassName="account-drawer">
      {job && <><Alert type="info" message="离开账号管理页会停止后续操作，已发送的请求仍会完成。不会自动重放失败请求。" /><Progress percent={Math.round(job.rows.length * 100 / job.total)} status={running ? 'active' : count('failed') ? 'exception' : 'normal'} /><Space wrap style={{ marginBottom: 16 }}><Tag color="green">成功 {count('success')}</Tag><Tag color="red">失败 {count('failed')}</Tag><Tag>未执行 {count('cancelled')}</Tag>{running ? <Button onClick={() => { if (active.current) active.current.cancelled = true; }}>取消后续操作</Button> : <>{[['failed', '重试失败项'], ['cancelled', '继续未执行项']].map(([status, label]) => <Button key={status} disabled={!count(status)} onClick={() => ask(job.operation.action, job.rows.filter(r => r.status === status).map(r => r.uid))}>{label}</Button>)}</>}</Space><Table rowKey="uid" size="small" dataSource={job.rows} pagination={{ pageSize: 25, showSizeChanger: false }} columns={[{ title: 'UID', dataIndex: 'uid' }, { title: '结果', dataIndex: 'status', width: 85, render: s => <Tag color={s === 'success' ? 'green' : s === 'failed' ? 'red' : undefined}>{s === 'success' ? '成功' : s === 'failed' ? '失败' : '未执行'}</Tag> }, { title: '详情', dataIndex: 'detail' }]} /></>}
    </Drawer>
    {transfer && <AccountTransfer api={api} mode={transfer.mode} uids={transfer.uids} onClose={() => setTransfer(null)} onImported={reload} />}
  </div>;
}
