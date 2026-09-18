import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  App, Button, Card, Drawer, Empty, Input, Popconfirm, Progress,
  Space, Spin, Table, Tag, Tooltip, Typography,
} from 'antd';
import {
  CheckCircleOutlined, ClockCircleOutlined, GiftOutlined, ReloadOutlined, RocketOutlined,
  SendOutlined, ThunderboltOutlined, TrophyOutlined,
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

/**
 * GrowthTasks 成长任务中心：
 * - 账号列表（复用 /status 的账号数据，仅 CN 账号可操作）
 * - 单账号任务表格：进度、奖励、一键完成（自动执行动作链 + 异步计分回读 + 自动领奖）
 * - 顶部手动触发：旅行巡检 / 活跃上报 / 全部账号一键完成
 */
export default function GrowthTasks({ api, data, refresh }) {
  const { message, modal } = App.useApp();
  const [loadingUid, setLoadingUid] = useState(null);
  const [tasks, setTasks] = useState(null);
  const [tasksLoading, setTasksLoading] = useState(false);
  const [autoActions, setAutoActions] = useState({});
  const [search, setSearch] = useState('');
  const [autoAllRunning, setAutoAllRunning] = useState(false);
  const [autoAllResults, setAutoAllResults] = useState(null);
  const [travelRunning, setTravelRunning] = useState(false);
  const [activityRunning, setActivityRunning] = useState(false);
  const [drawerUid, setDrawerUid] = useState('');
  const tasksUid = () => drawerUid;

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
    setLoadingUid(uid);
    setTasks(null);
    setAutoActions({});
    loadTasks(uid).finally(() => setLoadingUid(null));
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

  // runTaskAuto 一键完成单个任务（含自动领奖，后端有界轮询等异步计分）。
  const runTaskAuto = async (uid, taskCode) => {
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
    }
  };

  // runAutoAll 一键完成该账号全部可自动任务（长任务，后端 5 分钟超时后台继续）。
  const runAutoAll = async uid => {
    const hide = message.loading('一键完成全部任务执行中（多任务 × 节流，约 2-4 分钟）…', 0);
    try {
      const result = await api(`/admin/account/${uid}/tasks/auto_all`, { method: 'POST' });
      hide();
      const results = result.results || [];
      const claimed = results.filter(item => item.claimed).length;
      const done = results.filter(item => item.ok).length;
      modal.success({
        title: '一键完成结束',
        width: 640,
        content: (
          <div style={{ maxHeight: 360, overflow: 'auto', marginTop: 12 }}>
            {results.map(item => (
              <div key={item.task_code} style={{ marginBottom: 6 }}>
                <Text code>{item.task_code}</Text>{' '}
                {item.ok ? <Tag color={item.claimed ? 'green' : 'blue'}>{item.claimed ? '已领奖' : item.skipped ? '跳过' : '完成'}</Tag>
                  : <Tag color="red">失败</Tag>}
                <Text type="secondary">{item.message}</Text>
              </div>
            ))}
          </div>
        ),
        okText: `完成（${done}/${results.length} 项，${claimed} 项自动领奖）`,
      });
      loadTasks(uid);
      refresh();
    } catch (error) {
      hide();
      message.error(error.message);
    }
  };

  // runAllAccounts 全部账号批跑。
  const runAllAccounts = async () => {
    setAutoAllRunning(true);
    setAutoAllResults(null);
    const hide = message.loading('全部账号一键完成执行中（耗时较长，请勿关闭页面）…', 0);
    try {
      const result = await api('/admin/tasks/auto_all', { method: 'POST' });
      hide();
      setAutoAllResults(result.results || []);
      const total = (result.results || []).length;
      message.success(`已完成 ${total} 个账号的成长任务`);
      refresh();
    } catch (error) {
      hide();
      message.error(error.message);
    } finally {
      setAutoAllRunning(false);
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
          <Button size="small" icon={<GiftOutlined />} onClick={() => { setDrawerUid(record.uid); openTasks(record.uid); }}>任务</Button>
          <Popconfirm
            title="一键完成全部可自动任务"
            description="含 17 项任务动作（数条真实短对话），约 2-4 分钟"
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
                ? <Button size="small" type="primary" icon={<TrophyOutlined />} onClick={() => claimTask(tasksUid(), task.task_code)}>领取</Button>
                : <Button size="small" icon={<ThunderboltOutlined />} onClick={() => runTaskAuto(tasksUid(), task.task_code)}>一键完成</Button>}
            </Tooltip>
          )}
          {task.claimable && !autoActions[task.task_code] && (
            <Button size="small" type="primary" icon={<TrophyOutlined />} onClick={() => claimTask(tasksUid(), task.task_code)}>领取</Button>
          )}
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card title="成长任务运营" size="small">
        <Space wrap>
          <Button icon={<SendOutlined />} loading={activityRunning} onClick={runActivity}>立即活跃上报（全部账号）</Button>
          <Button icon={<ClockCircleOutlined />} loading={travelRunning} onClick={runTravel}>立即旅行巡检（全部账号）</Button>
          <Popconfirm
            title="对全部 CN 账号执行一键完成"
            description="每账号 17 项任务 × 节流，账号多时耗时很长；后台执行，可稍后刷新"
            onConfirm={runAllAccounts}
          >
            <Button type="primary" icon={<RocketOutlined />} loading={autoAllRunning}>全部账号一键完成</Button>
          </Popconfirm>
          <Text type="secondary">一键完成 ≈ +1950 积分 +78 能量 / 新账号；Expert_Philanthropy（真实捐款）无法自动完成</Text>
        </Space>
      </Card>

      {autoAllResults && (
        <Card title="全部账号执行结果" size="small">
          <div style={{ maxHeight: 300, overflow: 'auto' }}>
            {(autoAllResults || []).map(account => (
              <div key={account.uid} style={{ marginBottom: 8 }}>
                <Space>
                  <Text code>{account.uid}</Text>
                  {account.ok
                    ? <Tag color="green">{(account.results || []).filter(item => item.ok).length}/{(account.results || []).length} 项完成</Tag>
                    : <Tag color="red">{account.error}</Tag>}
                </Space>
              </div>
            ))}
            {!autoAllResults.length && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有可执行的账号" />}
          </div>
        </Card>
      )}

      <Card
        title="账号列表（仅中国区）" size="small"
        extra={(
          <Space>
            <Input allowClear placeholder="搜索 uid / 昵称" style={{ width: 220 }} value={search} onChange={e => setSearch(e.target.value)} />
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
              description="约 2-4 分钟（含真实短对话）"
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
          个别任务（如需真实客户端交互的 Expert_Philanthropy）无法自动完成，请按任务说明操作。
        </Paragraph>
      </Drawer>
    </Space>
  );
}
