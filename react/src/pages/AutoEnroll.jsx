import React, { useEffect, useRef, useState } from 'react';
import { Alert, Button, Card, Checkbox, Collapse, InputNumber, Popconfirm, Progress, Select, Space, Tabs, Tag, Typography, message } from 'antd';
import { PlayCircleOutlined, PauseCircleOutlined, ReloadOutlined, SearchOutlined, SettingOutlined } from '@ant-design/icons';
import AutoEnrollAdvanced from './AutoEnrollAdvanced';
import WatchCard from './WatchCard';
import SMSPlatforms from './SMSPlatforms';
import { compatibleGroups, createTaskPoller, platformNames, readyPlatforms, taskPlatformOptions, taskProgress, validTaskPlatform } from '../lib/autoEnroll.js';
import './autoEnroll.css';
const { Text } = Typography;
const Field = ({ label, children, wide }) => <div className={`enroll-field ${wide ? 'enroll-field-wide' : ''}`}><label>{label}</label>{children}</div>;

export default function AutoEnroll({ api, config, refreshConfig, onSaveHaozhuma, onToggleGrowthTasks }) {
  const hz = config.sms?.haozhuma || {};
  const ready = readyPlatforms(hz);
  const [tab, setTab] = useState('manual');
  const [status, setStatus] = useState(null);
  const [statusError, setStatusError] = useState('');
  const [actionError, setActionError] = useState('');
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [provider, setProvider] = useState('auto');
  const [count, setCount] = useState(5);
  const [workers, setWorkers] = useState(3);
  const [pollCount, setPollCount] = useState(18);
  const [maxAttempts, setMaxAttempts] = useState(null);
  const [groups, setGroups] = useState(['default']);
  const [groupOptions, setGroupOptions] = useState([]);
  const [groupError, setGroupError] = useState('');
  const [advanced, setAdvanced] = useState(null);
  const [initialPicker, setInitialPicker] = useState(null);
  const h5Ready = hz.h5?.has === true && hz.h5?.expired !== true;
  const [follow, setFollow] = useState(true);
  const poller = useRef(null);
  const logBox = useRef(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    const p = createTaskPoller(() => api('/admin/account/sms/auto-enroll'), body => { setStatus(body); setStatusError(''); }, e => setStatusError(e.message));
    poller.current = p; p.refresh();
    return () => { alive.current = false; p.close(); poller.current = null; };
  }, [api]);
  useEffect(() => {
    let active = true;
    api('/admin/groups').then(body => {
      if (!active) return;
      const names = compatibleGroups(body.groups).map(g => g.name);
      setGroupOptions(names.map(value => ({ value, label: value })));
      setGroups(cur => cur.filter(g => names.includes(g)).length ? cur.filter(g => names.includes(g)) : names.includes('default') ? ['default'] : names.slice(0, 1));
    }).catch(e => { if (active) setGroupError(e.message); });
    return () => { active = false; };
  }, [api]);
  useEffect(() => { setProvider(cur => validTaskPlatform(cur, ready)); }, [ready.haozhuma, ready.ecode]);
  useEffect(() => { if (follow && logBox.current) logBox.current.scrollTop = logBox.current.scrollHeight; }, [status?.logs, follow]);
  const busy = status?.running || status?.config_busy || (status?.held || 0) > 0 || starting || stopping;
  const progress = taskProgress(status);
  const loaded = status !== null;
  const refresh = async () => { await refreshConfig?.(); poller.current?.refresh(); };
  const start = async () => {
    if (![count, workers, pollCount].every(Number.isInteger) || !count || !workers || !pollCount || (maxAttempts !== null && (!Number.isInteger(maxAttempts) || maxAttempts < 1))) { message.error('目标数、并发和轮询次数需要填写正整数'); return; }
    if (provider !== validTaskPlatform(provider, ready)) { message.error('所选平台尚未配置完整'); return; }
    setStarting(true); setActionError('');
    try {
      await api('/admin/account/sms/auto-enroll', { method: 'POST', body: JSON.stringify({ count, workers, poll_count: pollCount, max_attempts: maxAttempts || 0, groups, provider, use_saved_platforms: true }) });
      setFollow(true); message.success('任务已启动');
    } catch (e) { setActionError(e.message); }
    finally { if (alive.current) { setStarting(false); poller.current?.refresh(); } }
  };
  const stop = async () => {
    setStopping(true); setActionError('');
    try { await api('/admin/account/sms/auto-enroll/stop', { method: 'POST', body: JSON.stringify({ reason: '控制台手动停止', pause_monitor: status?.source === 'monitor' }) }); message.info('已请求停止，正在结束在途号码'); }
    catch (e) { setActionError(e.message); }
    finally { if (alive.current) { setStopping(false); poller.current?.refresh(); } }
  };
  const saveAdvanced = async body => {
    const result = await api('/admin/config', { method: 'POST', body: JSON.stringify(body) });
    await refresh(); return result;
  };
  const stateLabel = status?.stopping && status?.running ? '正在停止' : status?.running ? (status.source === 'monitor' ? '监控正在加号' : '手动任务运行中') : status?.config_busy ? '正在收尾' : loaded ? '空闲' : '读取状态中';
  const taskCard = <>
    <div className="enroll-grid">
      <Card title="创建加号任务" extra={<Tag color={status?.running ? 'processing' : 'default'}>{stateLabel}</Tag>}>
        {!ready.haozhuma && !ready.ecode && <Alert showIcon type="info" message="先配置一个接码平台" description="在右侧选择豪猪或 E码云，保存凭据和接码项目后即可开始。" style={{ marginBottom: 20 }} />}
        {!busy ? <>
        <div className="enroll-fields">
          <Field label="本次任务使用的平台" wide><Select aria-label="本次任务平台" value={provider} onChange={setProvider} disabled={!!busy} options={taskPlatformOptions(ready)} /></Field>
          <Field label="目标账号数"><InputNumber aria-label="目标账号数" min={1} max={50} precision={0} value={count} onChange={setCount} disabled={!!busy} /></Field>
          <Field label="并发数"><InputNumber aria-label="并发数" min={1} max={8} precision={0} value={workers} onChange={setWorkers} disabled={!!busy} /></Field>
          <Field label="新账号所属分组" wide><Select aria-label="新账号所属分组" mode="multiple" value={groups} onChange={setGroups} disabled={!!busy || !!groupError} options={groupOptions} placeholder="留空使用 default" /></Field>
        </div>
        {groupError && <Alert type="warning" message="分组列表不可用，本次将使用 default" style={{ marginTop: 12 }} />}
        <Collapse style={{ marginTop: 20 }} items={[{ key: 'task', label: '等待与重试参数', children: <div className="enroll-fields">
          <Field label={`每号最多查询短信次数 · 约 ${pollCount * 5} 秒起`}><InputNumber aria-label="收码轮询次数" min={1} max={36} precision={0} value={pollCount} onChange={setPollCount} disabled={!!busy} /></Field>
          <Field label="最多尝试号码数"><InputNumber aria-label="最多尝试次数" min={1} max={2000} precision={0} value={maxAttempts} onChange={setMaxAttempts} disabled={!!busy} placeholder={`自动：${Math.max(20, count * 12)}`} /></Field>
          <div className="enroll-muted enroll-field-wide">平台网络故障的重试可能延长等待时间。费用按供应商结算规则计算，账号注册失败不代表短信免费。</div>
        </div> }]} />
        </> : <div className="enroll-settings-note">本批目标 {progress.want} 个账号 · {platformNames[status?.provider] || '自动选择'} · 并发 {status?.workers || workers}<div className="enroll-muted" style={{ marginTop: 8 }}>参数已锁定，下方可查看进度和日志。停止后可创建下一批任务。</div></div>}
        <div className="enroll-settings-note" style={{ marginTop: 20, marginBottom: 0 }}>
          <Space wrap size={[12, 8]}>
            <Text strong>豪猪对接码</Text>
            <Text type="secondary">{hz.sid ? `项目 ${hz.sid}` : '尚未选择项目'} · {(hz.uids || []).length ? `轮换池 ${(hz.uids || []).length} 个码` : hz.uid ? '已指定单码' : '平台自动分配'}</Text>
            <Button aria-label="搜索对接码" icon={<SearchOutlined />} disabled={!!busy || !loaded || !h5Ready} onClick={() => { setInitialPicker('uids'); setAdvanced('haozhuma'); }}>搜索对接码</Button>
            <Button type="link" disabled={!!busy || !loaded} onClick={() => setAdvanced('haozhuma')}>项目与取号设置</Button>
          </Space>
          <div className="enroll-muted" style={{ marginTop: 8 }}>{!h5Ready ? '搜索需要有效的豪猪 H5 会话，请在右侧「豪猪 · 配置 → 豪猪增强功能」中保存会话；也可进入项目与取号设置手动填码。' : '按码、运营商或省份筛选，查看价格与库存。选择只回填草稿，保存后才生效；仅作用于豪猪。'}</div>
        </div>
        <div className="enroll-actions">
          <Button aria-label="开始加号" type="primary" icon={<PlayCircleOutlined />} loading={starting} disabled={!!busy || !loaded || !!statusError || (!ready.haozhuma && !ready.ecode)} onClick={start}>开始加号</Button>
          <Button aria-label={status?.source === 'monitor' ? '暂停监控并停止本批' : '停止本批'} danger icon={<PauseCircleOutlined />} loading={stopping} disabled={!status?.running || status?.stopping} onClick={stop}>{status?.source === 'monitor' ? '暂停监控并停止本批' : '停止本批'}</Button>
          <Button type="text" icon={<SettingOutlined />} disabled={!!busy} onClick={() => setAdvanced(provider === 'ecode' ? 'ecode' : 'common')}>任务保护设置</Button>
        </div>
        <div className="enroll-muted" style={{ marginTop: 14 }}>平台和参数在启动时确定。需要更换平台时，请先停止当前批次；在途号码会按原平台收尾。</div>
        {!busy && <Checkbox style={{ marginTop: 18 }} checked={config.autoenroll_growth_tasks !== false} onChange={e => onToggleGrowthTasks?.(e.target.checked)}>成功后执行成长任务 <Text type="secondary">（会发起真实对话）</Text></Checkbox>}
      </Card>
      <SMSPlatforms settings={hz} running={!!busy} api={api} onSave={onSaveHaozhuma} onSaved={refresh} onAdvanced={setAdvanced} />
    </div>
    <Card title="当前 / 最近一批任务" extra={<Button type="text" icon={<ReloadOutlined />} onClick={() => poller.current?.refresh()}>刷新状态</Button>}>
      {status?.monitor_enabled && <Alert type="info" showIcon message="豪猪监控已开启，会按监控设置自动创建批次" action={<Button size="small" onClick={() => setTab('monitor')}>管理监控</Button>} />}
      {progress.want > 0 ? <>
        <div className="enroll-muted" style={{ marginTop: 14 }}>来源：{status.source === 'monitor' ? '豪猪监控' : '手动启动'} · 平台：{platformNames[status.provider] || '自动选择'} · 并发 {status.workers} · 每号查询 {status.poll_count} 次 · 最多尝试 {status.max_attempts} 次</div>
        <div className="enroll-stats">{[['成功账号', status.ok || 0], ['已收到验证码', status.received || 0], ['尝试号码', status.consumed || 0], ['在途号码', status.held || 0]].map(([label, value]) => <div className="enroll-stat" key={label}><Text type="secondary">{label}</Text><strong>{value}</strong></div>)}</div>
        {status.supplier_counts && <div className="enroll-muted" style={{ marginBottom: 12 }}>实际取号：豪猪 {status.supplier_counts.haozhuma || 0} 个 · E码云 {status.supplier_counts.ecode || 0} 个</div>}
        <Progress percent={progress.percent} status={status.running ? 'active' : progress.ok >= progress.want ? 'success' : 'normal'} format={() => `${progress.ok} / ${progress.want}`} />
        {status.stop_reason && <Alert style={{ marginTop: 16 }} showIcon type={status.stopping ? 'info' : 'warning'} message="本批结束原因" description={status.stop_reason} />}
      </> : <div className="enroll-muted" style={{ padding: '20px 0' }}>暂无任务。启动后会在这里显示实际平台、进度和在途号码。</div>}
      <div style={{ marginTop: 24 }}>
        <div className="enroll-log-toolbar"><Text strong>运行日志</Text><Text type="secondary">最近 {(status?.logs || []).length} 行</Text><Checkbox checked={follow} onChange={e => setFollow(e.target.checked)}>跟随最新日志</Checkbox></div>
        <div className="enroll-log" ref={logBox} onScroll={e => { const box = e.currentTarget; if (box.scrollHeight - box.clientHeight - box.scrollTop > 60) setFollow(false); }}>
          {(status?.logs || []).length ? status.logs.map((line, i) => <div key={i} style={{ color: line.includes('加号成功') ? '#86efac' : line.includes('失败') || line.includes('故障') ? '#fda4af' : undefined }}>{line}</div>) : <span style={{ color: '#94a3b8' }}>等待任务日志…</span>}
        </div>
      </div>
    </Card>
  </>;
  return <div className="enroll-page">

    {statusError && <Alert type="error" showIcon message="任务状态暂不可用，正在自动重试" description={statusError} />}
    {!status?.running && (status?.held || 0)>0 && <Alert type="warning" showIcon message={`有 ${status.held} 个遗留号码等待释放`} description="为保护在途号码，暂不能更换配置或启动新批次。" action={<Popconfirm title="释放豪猪账户全部占用号码？" description="此操作也会释放本服务账本之外的账户占用号。" onConfirm={async()=>{try{await api('/admin/account/sms/release-all',{method:'POST',body:'{}'});poller.current?.refresh();}catch(e){setActionError(e.message);}}}><Button danger size="small">释放占用</Button></Popconfirm>} />}
    {actionError && <Alert type="error" showIcon message="操作未完成" description={actionError} closable onClose={() => setActionError('')} />}
    <Tabs activeKey={tab} onChange={setTab} items={[
      { key: 'manual', label: '加号工作台', children: <Space direction="vertical" size={20} style={{ width: '100%' }}>{taskCard}</Space> },
      { key: 'monitor', label: '豪猪自动监控', children: <><Alert className="enroll-monitor-note" showIcon type="info" message="监控按计划自动加号，与手动任务共用执行队列" description="这里的预算是本地预估支出上限，不是给豪猪充值。费用以供应商账单为准。" /><WatchCard api={api} h5Ready={!!hz.h5?.has && !hz.h5?.expired} onConfigure={() => setTab('manual')} /></> },
    ]} />
    <AutoEnrollAdvanced open={!!advanced} provider={advanced} initialPicker={initialPicker} onClose={() => { setAdvanced(null); setInitialPicker(null); }} api={api} config={config} running={!!busy} onSaveSettings={saveAdvanced} />
  </div>;
}
