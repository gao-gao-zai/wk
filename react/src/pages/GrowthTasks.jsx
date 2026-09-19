import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  App, Alert, Button, Card, Drawer, Empty, Input, Popconfirm, Progress,
  Skeleton, Space, Spin, Table, Tag, Tooltip, Typography,
} from 'antd';
import {
  CheckCircleOutlined, ClockCircleOutlined, DownOutlined, GiftOutlined, LoadingOutlined,
  ReloadOutlined, RocketOutlined, SendOutlined, ThunderboltOutlined, TrophyOutlined,
  UpOutlined,
} from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// 任务状态标签：进度/领取状态的可读化。
function taskStatusTag(task) {
  if (task.claimed) return <Tag color="green">已领取</Tag>;
  if (task.claimable) return <Tag color="gold">可领取</Tag>;
  if (task.locked) return <Tag color="default">未解锁</Tag>;
  if (task.accept_status === 'accepted') return <Tag color="blue">进行中</Tag>;
  if (task.accept_status === 'not_accepted') return <Tag>未接受</Tag>;
  return <Tag color="default">{task.accept_status || '—'}</Tag>;
}

// 任务奖励文案。
function rewardText(task) {
  const parts = [];
  if (task.credit) parts.push(`+${task.credit}分`);
  if (task.energy) parts.push(`+${task.energy}能`);
  if (task.reward_buddy) parts.push('UR Buddy');
  return parts.join(' ') || '—';
}

// jobItemTag 单项结果的标签。
function jobItemTag(item) {
  if (!item.ok) return <Tag color="red">失败</Tag>;
  if (item.claimed) return <Tag color="green">已领奖</Tag>;
  if (item.skipped) return <Tag>跳过</Tag>;
  return <Tag color="blue">完成</Tag>;
}

/**
 * GrowthTasks 成长任务中心：
 * - 账号列表（复用 /status 的账号数据，仅 CN 账号可操作）
 * - 批跑进度：页面常驻卡片（进页面自动恢复显示；执行中实时轮询；完成后折叠为结果摘要）
 * - 单账号任务抽屉：进度、奖励、单任务一键完成（同步，约 10-15s）
 * - 顶部手动触发：旅行巡检 / 活跃上报
 */
export default function GrowthTasks({ api, data, refresh }) {
  const { message } = App.useApp();
  const [tasks, setTasks] = useState(null);
  const [tasksLoading, setTasksLoading] = useState(false);
  const [autoActions, setAutoActions] = useState({});
  const [search, setSearch] = useState('');
  const [travelRunning, setTravelRunning] = useState(false);
  const [activityRunning, setActivityRunning] = useState(false);
  const [drawerUid, setDrawerUid] = useState('');
  const [taskBusy, setTaskBusy] = useState(false);
  // 常驻进度卡片状态：jobSnap = 最近一次进度快照；dismissed = 用户手动关闭已完成卡片。
  const [jobSnap, setJobSnap] = useState(null);
  const [detailOpen, setDetailOpen] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const pollRef = useRef(null);
  // 一次性任务台账：默认收起（只显示汇总进度条），点击展开看账号明细。
  const [ledger, setLedger] = useState(null);
  const [ledgerLoading, setLedgerLoading] = useState(false);
  const [ledgerOpen, setLedgerOpen] = useState(false);
  // 开学季活动状态卡（活动期 2026-09-13 ~ 09-24，不在期后端返回 in_period=0 隐藏）。
  const [school, setSchool] = useState(null);
  const [schoolLoading, setSchoolLoading] = useState(false);
  const [schoolOpen, setSchoolOpen] = useState(false);
  const [schoolRunning, setSchoolRunning] = useState(false);

  // CN 账号（global 无成长任务体系）。
  const accounts = useMemo(
    () => (data.accounts || []).filter(account => account.region !== 'global'),
    [data],
  );
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return accounts;
    return accounts.filter(account =>
      (account.uid || '').toLowerCase().includes(q)
      || (account.nickname || '').toLowerCase().includes(q));
  }, [accounts, search]);

  // ---------------------------------------------------------------------------
  // job 轮询：跟踪进行中的任务；结束（done/failed）时停轮询 + 刷新数据。
  // 与弹窗方案不同：轮询是常开的（进页面即恢复），进度渲染在页面卡片里。
  // ---------------------------------------------------------------------------
  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      window.clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  const watchJob = useCallback(id => {
    stopPolling();
    setDismissed(false);
    let missed = 0;
    const tick = async () => {
      try {
        const snap = await api(`/admin/growth/jobs/${id}`);
        setJobSnap({ ...snap, _id: id });
        missed = 0;
        if (snap.phase && snap.phase !== 'running') {
          stopPolling();
          refresh();
          if (drawerUid) loadTasks(drawerUid);
        }
      } catch (error) {
        // 查询失败：连续 3 次才放弃（网络抖动容忍），期间保持最后快照。
        if (++missed >= 3) {
          stopPolling();
          setJobSnap(current => (current ? { ...current, _error: error.message } : null));
        }
      }
    };
    tick();
    pollRef.current = window.setInterval(tick, 2000);
  }, [api, refresh, stopPolling, drawerUid]);

  // 进页面：查活动 job，有则自动恢复常驻进度（含重启续跑的任务）。
  useEffect(() => {
    (async () => {
      try {
        const result = await api('/admin/growth/jobs');
        const jobs = result.jobs || [];
        if (jobs.length) watchJob(jobs[jobs.length - 1].id);
      } catch { /* 静默：老后端无此端点 */ }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 卸载清定时器。
  useEffect(() => () => stopPolling(), [stopPolling]);

  // loadLedger 拉取一次性任务台账（三态统计 + 明细）。
  // force=true 时带 refresh=1 绕过后端对账缓存（「刷新」按钮用）；
  // 普通加载（进页面/批跑结束）走缓存——对账是低频慢变数据，不必每次
  // 都打 85 号上游列表。
  const loadLedger = useCallback(async (force = false) => {
    setLedgerLoading(true);
    try {
      setLedger(await api(`/admin/growth/ledger${force ? '?refresh=1' : ''}`));
    } catch { /* 老后端无此端点：静默 */ }
    finally { setLedgerLoading(false); }
  }, [api]);

  // loadSchool 拉取开学季状态（活动期外 in_period_accounts=0 → 隐藏卡片）。
  const loadSchool = useCallback(async () => {
    setSchoolLoading(true);
    try {
      setSchool(await api('/admin/school/status'));
    } catch { /* 静默 */ }
    finally { setSchoolLoading(false); }
  }, [api]);

  // runSchoolNow 手动触发全账号开学季闭环。
  const runSchoolNow = useCallback(async () => {
    setSchoolRunning(true);
    try {
      const result = await api('/admin/school', { method: 'POST' });
      message.success(result.message || '开学季闭环已启动');
      window.setTimeout(loadSchool, 8000);
    } catch (error) {
      message.error(error.message);
    } finally {
      setSchoolRunning(false);
    }
  }, [api, message, loadSchool]);

  // 进页面 + job 结束时刷新台账。
  useEffect(() => { loadLedger(); loadSchool(); }, [loadLedger, loadSchool]);
  useEffect(() => {
    if (jobSnap && jobSnap.phase && jobSnap.phase !== 'running') loadLedger();
  }, [jobSnap?.phase]); // eslint-disable-line react-hooks/exhaustive-deps

  // loadTasks 拉取单账号任务列表。
  const loadTasks = useCallback(async uid => {
    setTasksLoading(true);
    try {
      const result = await api(`/admin/account/${uid}/tasks`);
      setTasks(result.tasks || []);
      setAutoActions(result.auto_actions || {});
    } catch (error) {
      message.error(error.message);
    } finally {
      setTasksLoading(false);
    }
  }, [api, message]);

  // openTasks 打开某账号的任务面板。
  const openTasks = useCallback(uid => {
    setDrawerUid(uid);
    setTasks(null);
    setAutoActions({});
    loadTasks(uid);
  }, [loadTasks]);

  // acceptAll 接受该账号全部未接受任务。
  const acceptAll = async uid => {
    try {
      const result = await api(`/admin/account/${uid}/tasks/accept_all`, { method: 'POST' });
      message.success(`已接受 ${result.accepted || 0} 项任务`);
      loadTasks(uid);
    } catch (error) {
      message.error(error.message);
    }
  };

  // claimTask 手动领取单任务奖励。
  const claimTask = async (uid, taskCode) => {
    try {
      const result = await api(`/admin/account/${uid}/tasks/claim`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ task_code: taskCode }),
      });
      if (result.already_claimed) {
        message.info('该奖励此前已领取');
      } else {
        message.success(`领取成功 +${result.credit || 0}分 +${result.energy || 0}能`);
      }
      loadTasks(uid);
    } catch (error) {
      message.error(error.message);
    }
  };

  // runTaskAuto 一键完成单个任务（同步等 10-15s：动作 + 异步计分回读 + 自动领奖）。
  const runTaskAuto = async (uid, taskCode) => {
    setTaskBusy(true);
    const hide = message.loading('任务执行中（含异步计分等待，约 10-15 秒）…', 0);
    try {
      const result = await api(`/admin/account/${uid}/tasks/auto`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ task_code: taskCode }),
      });
      hide();
      const progress = result.progress_before && result.progress_after
        ? `（${result.progress_before} → ${result.progress_after}）` : '';
      if (result.skipped) {
        message.info(result.message || '该任务已领取过奖励');
      } else if (result.claimed) {
        message.success(`${result.message}${progress}`);
      } else if (result.claimable) {
        message.warning(`${result.message}${progress}；自动领奖失败可手动重试`);
      } else {
        message.warning(`${result.message}${progress}`);
      }
      loadTasks(uid);
    } catch (error) {
      hide();
      message.error(error.message);
    } finally {
      setTaskBusy(false);
    }
  };

  // runAutoAll 一键完成该账号全部可自动任务（异步 job：202 立即返回 + 常驻进度卡片）。
  const runAutoAll = async uid => {
    try {
      const result = await api(`/admin/account/${uid}/tasks/auto_all`, { method: 'POST' });
      if (result.job_id) {
        message.success('任务已开始，后台执行中——可以离开本页');
        watchJob(result.job_id);
      }
    } catch (error) {
      message.error(error.message);
    }
  };

  // runAllAccounts 全部账号批跑（异步 job，进度按账号×任务项推进）。
  const runAllAccounts = async () => {
    try {
      const result = await api('/admin/tasks/auto_all', { method: 'POST' });
      if (result.job_id) {
        message.success(`批跑已开始（${result.total_accounts} 个账号），后台执行中——可以离开本页`);
        watchJob(result.job_id);
      } else if (result.message) {
        message.info(result.message);
      }
    } catch (error) {
      message.error(error.message);
    }
  };

  // runTravel / runActivity 手动触发巡检与上报。
  const runTravel = async () => {
    setTravelRunning(true);
    try {
      const result = await api('/admin/travel', { method: 'POST' });
      message.success(result.message || '旅行巡检已启动');
      window.setTimeout(refresh, 4000);
    } catch (error) {
      message.error(error.message);
    } finally {
      setTravelRunning(false);
    }
  };
  const runActivity = async () => {
    setActivityRunning(true);
    try {
      const result = await api('/admin/activity', { method: 'POST' });
      message.success(result.message || '活跃上报已启动');
    } catch (error) {
      message.error(error.message);
    } finally {
      setActivityRunning(false);
    }
  };

  const columns = [
    {
      title: '账号', key: 'uid', ellipsis: true,
      render: (_, record) => (
        <Space direction="vertical" size={0}>
          <Text code>{record.uid}</Text>
          <Text type="secondary">{record.nickname || '-'}</Text>
        </Space>
      ),
    },
    {
      title: '积分', dataIndex: 'credits', key: 'credits', width: 100, align: 'right',
      render: value => <Text>{Number(value || 0).toLocaleString()}</Text>,
    },
    {
      title: '状态', key: 'status', width: 90,
      render: (_, record) => {
        if (record.disabled) return <Tag color="red">禁用</Tag>;
        if (record.cooling) return <Tag color="orange">冷却</Tag>;
        return <Tag color="green">正常</Tag>;
      },
    },
    {
      title: '操作', key: 'actions', width: 220, align: 'center',
      render: (_, record) => (
        <Space>
          <Button size="small" icon={<GiftOutlined />} onClick={() => openTasks(record.uid)}>任务</Button>
          <Popconfirm
            title="一键完成全部可自动任务"
            description="含 17 项任务动作（数条真实短对话），约 2-4 分钟；后台执行，可离开页面"
            onConfirm={() => runAutoAll(record.uid)}
          >
            <Button size="small" type="primary" icon={<RocketOutlined />}>一键完成</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  const taskColumns = [
    {
      title: '任务', key: 'task', ellipsis: true,
      render: (_, task) => (
        <Space direction="vertical" size={0} style={{ width: '100%' }}>
          <Space size={6}>
            <Text strong ellipsis style={{ maxWidth: 200 }}>{task.title || task.task_code}</Text>
            {taskStatusTag(task)}
          </Space>
          <Text type="secondary" ellipsis style={{ maxWidth: 260 }}>{task.task_desc || task.description || task.task_code}</Text>
        </Space>
      ),
    },
    {
      title: '进度', key: 'progress', width: 130,
      render: (_, task) => {
        if (task.claimed) return <Text type="secondary">已完成</Text>;
        if (!task.target) return <Text type="secondary">—</Text>;
        const percent = Math.min(100, Math.round((task.current / task.target) * 100));
        return (
          <Space direction="vertical" size={0} style={{ width: '100%' }}>
            <Progress percent={percent} size="small" showInfo={false} />
            <Text type="secondary">{task.current}/{task.target}</Text>
          </Space>
        );
      },
    },
    { title: '奖励', key: 'reward', width: 130, render: (_, task) => <Text>{rewardText(task)}</Text> },
    {
      title: '操作', key: 'actions', width: 170, align: 'center',
      render: (_, task) => (
        <Space>
          {autoActions[task.task_code] && !task.claimed && (
            <Tooltip title={task.claimable ? '进度已达标，直接领取奖励' : '执行行为链并等待计分，达标自动领奖'}>
              {task.claimable
                ? <Button size="small" type="primary" icon={<TrophyOutlined />} disabled={taskBusy} onClick={() => claimTask(drawerUid, task.task_code)}>领取</Button>
                : <Button size="small" icon={<ThunderboltOutlined />} disabled={taskBusy} onClick={() => runTaskAuto(drawerUid, task.task_code)}>一键完成</Button>}
            </Tooltip>
          )}
          {task.claimable && !autoActions[task.task_code] && (
            <Button size="small" type="primary" icon={<TrophyOutlined />} disabled={taskBusy} onClick={() => claimTask(drawerUid, task.task_code)}>领取</Button>
          )}
        </Space>
      ),
    },
  ];

  // ---------------------------------------------------------------------------
  // 常驻进度卡片：进行中展开显示；完成后折叠为摘要（可关闭）；失败带错误提示。
  // ---------------------------------------------------------------------------
  const snap = jobSnap;
  const running = snap && snap.phase === 'running';
  const failed = snap && snap.phase === 'failed';
  const percent = snap && snap.total
    ? Math.min(100, Math.round((snap.done / snap.total) * 100))
    : (snap ? 95 : 0);
  const items = (snap?.results) || [];
  const doneItems = items.filter(item => item.ok);
  const claimedCount = items.filter(item => item.claimed).length;
  // 完成且用户没关过 → 显示摘要卡；用户关了（dismissed）→ 不显示。
  const showCard = snap && (running || failed || !dismissed);
  // 进度文案按模式区分：all = 账号粒度（N/M 个账号）；account = 任务项粒度。
  const isAllMode = snap?.mode === 'all';
  const progressLabel = snap && snap.total
    ? (isAllMode ? `${snap.done}/${snap.total} 个账号` : `${snap.done}/${snap.total} 项`)
    : `${snap?.done ?? 0}/…`;
  const summaryText = isAllMode
    ? `共 ${snap.total} 个账号：${items.length} 项结果（${doneItems.length} 完成、${claimedCount} 项自动领奖、${items.length - doneItems.length} 失败/跳过）`
    : `共 ${items.length} 项：${doneItems.length} 完成、${claimedCount} 项自动领奖、${items.length - doneItems.length} 失败/跳过`;

  const progressCard = showCard ? (
    <Card
      size="small"
      style={{ borderLeft: running ? '3px solid #1677ff' : failed ? '3px solid #ff4d4f' : '3px solid #52c41a' }}
      title={running
        ? <Space><LoadingOutlined spin /><span>批跑执行中</span><Text type="secondary" style={{ fontWeight: 400 }}>后台运行，可离开本页</Text></Space>
        : failed ? <span style={{ color: '#cf1322' }}>批跑失败</span> : <Space><CheckCircleOutlined style={{ color: '#52c41a' }} /><span>批跑完成</span></Space>}
      extra={running ? null : (
        <Button size="small" type="text" onClick={() => setDismissed(true)}>关闭</Button>
      )}
    >
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        <Progress
          percent={percent}
          status={failed ? 'exception' : (running ? 'active' : 'success')}
          format={() => progressLabel}
        />
        <Space wrap>
          {running && snap.current && (
            <Text type="secondary">正在执行：<Text code>{snap.current}</Text></Text>
          )}
          {snap.elapsed && <Text type="secondary">已用时 {snap.elapsed}</Text>}
          {running && snap.eta && <Text type="secondary">预计剩余 {snap.eta}</Text>}
          {!running && (
            <Text type="secondary">{summaryText}</Text>
          )}
        </Space>
        {failed && snap.error && <Alert type="error" showIcon message={snap.error} style={{ padding: '4px 12px' }} />}
        {snap._error && <Alert type="warning" showIcon message={`进度查询中断（${snap._error}），最后状态如下`} style={{ padding: '4px 12px' }} />}
        {items.length > 0 && (
          <>
            <Button
              size="small" type="link" style={{ padding: 0, height: 'auto' }}
              onClick={() => setDetailOpen(open => !open)}
              icon={detailOpen ? <UpOutlined /> : <DownOutlined />}
            >
              {detailOpen ? '收起明细' : `展开明细（${items.length} 项）`}
            </Button>
            {detailOpen && (
              <div style={{ maxHeight: 280, overflow: 'auto', background: '#fafafa', padding: '8px 12px', borderRadius: 6 }}>
                {items.map((item, index) => (
                  <div key={`${item.uid || ''}-${item.task_code}-${index}`} style={{ marginBottom: 6 }}>
                    {item.uid && <Text code style={{ fontSize: 12 }}>{item.uid.slice(0, 8)}…</Text>}{' '}
                    {item.task_code && <Text code style={{ fontSize: 12 }}>{item.task_code}</Text>}{' '}
                    {jobItemTag(item)}
                    <Text type="secondary" style={{ fontSize: 12 }}>{item.message}</Text>
                  </div>
                ))}
              </div>
            )}
          </>
        )}
      </Space>
    </Card>
  ) : null;

  // ---------------------------------------------------------------------------
  // 一次性任务台账卡片：默认一行汇总进度条；点击展开账号明细表。
  // ---------------------------------------------------------------------------
  const statusTag = status => ({
    done: <Tag color="green">已完成</Tag>,
    partial: <Tag color="orange">部分完成</Tag>,
    not_started: <Tag color="default">未开始</Tag>,
  }[status] || <Tag>{status}</Tag>);

  const ledgerColumns = [
    {
      title: '账号', key: 'uid', ellipsis: true,
      render: (_, record) => (
        <Space direction="vertical" size={0}>
          <Text code>{record.uid}</Text>
          <Text type="secondary">{record.nickname || '-'}</Text>
        </Space>
      ),
    },
    {
      title: '一次性任务', key: 'status', width: 120,
      render: (_, record) => statusTag(record.status),
    },
    {
      title: '进度', key: 'progress', width: 170,
      render: (_, record) => (
        <Space direction="vertical" size={0} style={{ width: '100%' }}>
          <Progress percent={record.total_count ? Math.round((record.done_count / record.total_count) * 100) : 0} size="small" showInfo={false} />
          <Text type="secondary">{record.done_count}/{record.total_count} 项</Text>
        </Space>
      ),
    },
    {
      title: '任务积分收益', dataIndex: 'credit', key: 'credit', width: 130, align: 'right',
      render: value => <Text type={value > 0 ? 'success' : undefined}>{value > 0 ? `+${value} 分` : '—'}</Text>,
    },
    {
      title: '能量收益', dataIndex: 'energy', key: 'energy', width: 100, align: 'right',
      render: value => value > 0 ? <Text>+{value} 能</Text> : <Text type="secondary">—</Text>,
    },
    {
      title: '最近领取', dataIndex: 'last_at', key: 'last_at', width: 160,
      render: value => value ? <Text type="secondary">{value.replace('T', ' ').slice(0, 19)}</Text> : <Text type="secondary">—</Text>,
    },
  ];

  // 汇总进度：全池一次性任务的总完成度（已完成任务数 / 账号数×17）。
  const ledgerTotalItems = ledger ? ledger.total_accounts * (ledger.accounts[0]?.total_count || 17) : 0;
  const ledgerDoneItems = ledger
    ? ledger.accounts.reduce((sum, account) => sum + account.done_count, 0)
    : 0;
  const ledgerPercent = ledgerTotalItems
    ? Math.round((ledgerDoneItems / ledgerTotalItems) * 100)
    : 0;

  const ledgerCard = (
    <Card
      size="small" title="一次性任务台账"
      extra={(
        <Space>
          {ledger && (
            <Button size="small" type="text" onClick={() => setLedgerOpen(open => !open)} icon={ledgerOpen ? <UpOutlined /> : <DownOutlined />}>
              {ledgerOpen ? '收起明细' : '展开明细'}
            </Button>
          )}
          <Button size="small" icon={<ReloadOutlined />} loading={ledgerLoading} onClick={() => loadLedger(true)}>刷新</Button>
        </Space>
      )}
    >
      {/* 加载态：骨架占位（对账要拉全部账号任务列表，秒级），避免空白/跳变 */}
      {!ledger ? (
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Skeleton active paragraph={{ rows: 1 }} title={false} />
          <Skeleton.Button active block size="small" shape="round" />
        </Space>
      ) : (
        <>
          {/* 默认态：一条汇总进度条 + 三态计数 + 总收益，一眼看全貌 */}
          <Space direction="vertical" size={6} style={{ width: '100%' }}>
            <Progress
              percent={ledgerPercent}
              format={() => `${ledgerDoneItems}/${ledgerTotalItems} 项（${ledgerPercent}%）`}
            />
            <Space wrap>
              <Tag color="green">已完成 {ledger.done_accounts}</Tag>
              <Tag color="orange">部分完成 {ledger.partial_accounts}</Tag>
              <Tag>未开始 {ledger.not_started}</Tag>
              <Tag color="blue">共 {ledger.total_accounts} 个账号</Tag>
              <Text strong style={{ color: '#389e0d' }}>任务总收益 +{ledger.total_credit} 积分</Text>
              <Text type="secondary">+{ledger.total_energy} 能量</Text>
            </Space>
          </Space>
          {ledgerOpen && (
        <>
          <Table
            style={{ marginTop: 12 }}
            rowKey="uid" size="small" columns={ledgerColumns} dataSource={ledger.accounts}
            pagination={{ pageSize: 10, showTotal: total => `共 ${total} 个账号` }}
            expandable={{
              rowExpandable: record => (record.tasks || []).length > 0,
              expandedRowRender: record => (
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                  {(record.tasks || []).map(task => (
                    <Tooltip key={task.task_code} title={task.at ? `${task.at.replace('T', ' ').slice(0, 19)} 领取${task.credit ? ` +${task.credit}分` : ''}${task.energy ? ` +${task.energy}能` : ''}` : '本网关部署前/手动完成（无时间记录）'}>
                      <Tag color={task.at ? 'green' : 'default'}>{task.task_code}{task.credit ? ` +${task.credit}` : ''}</Tag>
                    </Tooltip>
                  ))}
                </div>
              ),
            }}
          />
          <Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
            台账记录本网关自动领取的任务奖励（含领取时间与积分）；「部分完成/未开始」的判定实时对账上游任务状态。
            任务积分收益只统计经本网关领取的部分，此前手动完成的按 0 计。
          </Paragraph>
        </>
          )}
        </>
      )}
    </Card>
  );

  // 筛选后的总余额（搜索命中哪些账号就统计哪些）。
  const filteredCredits = filtered.reduce((sum, account) => sum + Number(account.credits || 0), 0);

  // ---------------------------------------------------------------------------
  // 开学季活动卡片（活动期 2026-09-13 ~ 09-24；结束自动隐藏）。
  // ---------------------------------------------------------------------------
  const SCHOOL_TASK_LABELS = {
    share_invite: '分享活动', desktop_chat_1_time: '桌面端体验', chat_3_times: '对话×3',
    expert_use: '召唤专家', task_student_verify: '学生认证（需人工）',
  };
  const schoolStatusTag = st => ({
    claimed: <Tag color="green">已领</Tag>,
    completed: <Tag color="cyan">待领取</Tag>,
    in_progress: <Tag color="blue">进行中</Tag>,
    pending: <Tag color="default">未开始</Tag>,
  }[st] || <Tag>{st}</Tag>);
  const totalVouchers = (school?.accounts || []).reduce((sum, acc) => sum + (acc.vouchers?.length || 0), 0);

  const schoolCard = !school ? (
    // 加载骨架：状态卡要并发拉全部账号的任务矩阵 + 抽奖余额 + 券码。
    <Card size="small" style={{ borderLeft: '3px solid #eb2f96' }} title="🎒 开学季活动（至 09-24）">
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        <Skeleton active paragraph={{ rows: 1 }} title={false} />
        <Skeleton.Button active block size="small" shape="round" />
      </Space>
    </Card>
  ) : school.in_period_accounts > 0 ? (
    <Card
      size="small"
      style={{ borderLeft: '3px solid #eb2f96' }}
      title="🎒 开学季活动（至 09-24）"
      extra={(
        <Space>
          <Button size="small" type="text" onClick={() => setSchoolOpen(open => !open)} icon={schoolOpen ? <UpOutlined /> : <DownOutlined />}>
            {schoolOpen ? '收起明细' : '展开明细'}
          </Button>
          <Button size="small" icon={<ReloadOutlined />} loading={schoolLoading} onClick={loadSchool}>刷新</Button>
          <Button size="small" type="primary" danger ghost icon={<RocketOutlined />} loading={schoolRunning} onClick={runSchoolNow}>立即执行闭环</Button>
        </Space>
      )}
    >
      <Space wrap>
        <Tag color="pink">活动进行中</Tag>
        <Tag color="blue">{school.in_period_accounts} 个账号在期</Tag>
        <Tag color={school.pending_tasks > 0 ? 'orange' : 'green'}>待办 {school.pending_tasks} 项</Tag>
        {totalVouchers > 0 && <Tag color="gold">🎁 已中 {totalVouchers} 张实体券</Tag>}
        <Text type="secondary">每日 ~200 分/号 + 抽奖；签到排程末尾自动执行（9/21 点）</Text>
      </Space>
      {schoolOpen && (
        <Table
          style={{ marginTop: 12 }}
          rowKey="uid" size="small"
          dataSource={school.accounts.filter(acc => acc.in_period)}
          pagination={{ pageSize: 10, showTotal: total => `共 ${total} 个账号` }}
          columns={[
            {
              title: '账号', key: 'uid', ellipsis: true,
              render: (_, acc) => (
                <Space direction="vertical" size={0}>
                  <Text code>{acc.uid}</Text>
                  <Text type="secondary">{acc.nickname || '-'}</Text>
                </Space>
              ),
            },
            {
              title: '任务', key: 'tasks',
              render: (_, acc) => (
                <Space wrap size={4}>
                  {(acc.tasks || []).map(t => (
                    <Tooltip key={t.task_code} title={`${t.progress}/${t.target_count || '-'}`}>
                      <span>
                        {SCHOOL_TASK_LABELS[t.task_code] || t.task_code}
                        {' '}
                        {schoolStatusTag(t.status)}
                      </span>
                    </Tooltip>
                  ))}
                </Space>
              ),
            },
            {
              title: '抽奖余额', dataIndex: 'chances', key: 'chances', width: 90, align: 'center',
              render: value => <Tag color={value > 0 ? 'gold' : 'default'}>{value || 0} 次</Tag>,
            },
            {
              title: '券码', key: 'vouchers', width: 220,
              render: (_, acc) => (acc.vouchers || []).length
                ? <Tooltip title={acc.vouchers.map(v => `${v.prize_name}: ${v.code}`).join('\n')}>
                    <Text code>{acc.vouchers.map(v => v.prize_name).join('、')}</Text>
                  </Tooltip>
                : <Text type="secondary">—</Text>,
            },
          ]}
        />
      )}
    </Card>
  ) : null; // 活动期外（in_period_accounts=0）：不显示卡片

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {progressCard}
      {ledgerCard}
      {schoolCard}

      <Card title="成长任务运营" size="small">
        <Space wrap>
          <Button icon={<SendOutlined />} loading={activityRunning} onClick={runActivity}>立即活跃上报（全部账号）</Button>
          <Button icon={<ClockCircleOutlined />} loading={travelRunning} onClick={runTravel}>立即旅行巡检（全部账号）</Button>
          <Popconfirm
            title="对全部 CN 账号执行一键完成"
            description="后台异步执行（可离开页面），页面顶部实时显示进度"
            onConfirm={runAllAccounts}
          >
            <Button type="primary" icon={<RocketOutlined />} disabled={running}>全部账号一键完成</Button>
          </Popconfirm>
          {running && <Text type="secondary">批跑进行中，结束后可再次发起</Text>}
          <Text type="secondary">一键完成 ≈ +1950 积分 +78 能量 / 新账号；Expert_Philanthropy（真实捐款）无法自动完成</Text>
        </Space>
      </Card>

      <Card
        title={`账号列表（仅中国区，筛选后 ${filtered.length}/${accounts.length}）`} size="small"
        extra={(
          <Space>
            <Input allowClear placeholder="搜索 uid / 昵称" style={{ width: 220 }} value={search} onChange={e => setSearch(e.target.value)} />
            <Text type="secondary">剩余余额 {filteredCredits.toLocaleString()}</Text>
            <Button icon={<ReloadOutlined />} onClick={refresh}>刷新</Button>
          </Space>
        )}
      >
        <Table
          rowKey="uid" size="small" columns={columns} dataSource={filtered}
          pagination={{ pageSize: 20, showSizeChanger: true, showTotal: total => `共 ${total} 个账号` }}
        />
      </Card>

      <Drawer
        title={(
          <Space>
            <span>成长任务</span>
            {drawerUid && <Text code>{drawerUid}</Text>}
          </Space>
        )}
        width={720} open={!!drawerUid} onClose={() => setDrawerUid('')}
        extra={drawerUid && (
          <Space>
            <Button icon={<CheckCircleOutlined />} onClick={() => acceptAll(drawerUid)}>全部接受</Button>
            <Button icon={<ReloadOutlined />} onClick={() => loadTasks(drawerUid)}>刷新</Button>
            <Popconfirm
              title="一键完成全部可自动任务"
              description="后台异步执行（可离开页面），页面顶部实时显示进度"
              onConfirm={() => runAutoAll(drawerUid)}
            >
              <Button type="primary" icon={<RocketOutlined />}>一键完成全部</Button>
            </Popconfirm>
          </Space>
        )}
      >
        {tasksLoading && !tasks ? (
          <div style={{ textAlign: 'center', padding: 48 }}><Spin /></div>
        ) : !tasks || !tasks.length ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无任务数据" />
        ) : (
          <Table
            rowKey="task_code" size="small" columns={taskColumns} dataSource={tasks}
            pagination={false}
          />
        )}
        <Paragraph type="secondary" style={{ marginTop: 16 }}>
          说明：任务计分为上游异步处理，「一键完成」执行后会等待计分落定并自动领奖；
          全量任务在后台异步执行，关闭页面不影响；个别任务（如需真实捐款的
          Expert_Philanthropy）无法自动完成。
        </Paragraph>
      </Drawer>
    </Space>
  );
}
