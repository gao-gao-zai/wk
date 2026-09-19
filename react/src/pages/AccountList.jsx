import React, { useEffect, useMemo, useState } from 'react';
import {
  App, Button, Card, Input, Select, Space, Statistic, Table, Tag, Tooltip, Typography,
} from 'antd';
import {
  ApartmentOutlined, CheckCircleOutlined, DeleteOutlined, ReloadOutlined, StopOutlined,
  ThunderboltOutlined, UnlockOutlined,
} from '@ant-design/icons';

const { Text } = Typography;
const fmt = value => Number(value || 0).toLocaleString();

// 冷却原因文案：与后端 CoolKind.String() 的取值一一对应。
const COOL_KIND_LABEL = {
  hard_credit: '积分耗尽',
  soft_rate: '429 限流',
  rate_limit: '上游限流',
};

// parseDeadline 解析后端 time.Time 字段为毫秒时间戳；零值返回 0。
// 注意：Go 的 time.Time 是结构体，`omitempty` 对它无效 —— 未设置时后端会上送
// "0001-01-01T00:00:00Z"，所以必须显式识别零值，否则正常账号会被误判为"熔断中"。
function parseDeadline(value) {
  if (!value) return 0;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return 0;
  return date.getUTCFullYear() <= 1 ? 0 : date.getTime();
}

// token 剩余有效期文案：后端给的是 Unix 秒。10 分钟内标红提示即将失效。
function tokenExpiryLabel(expiresAt) {
  if (!expiresAt) return null;
  const remaining = expiresAt - Math.floor(Date.now() / 1000);
  if (remaining <= 0) return { text: '已过期', danger: true };
  return { text: `${fmtDuration(remaining)}后过期`, danger: remaining < 600 };
}

// fmtDuration 把秒数格式化成紧凑的中文时长文案。
function fmtDuration(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds) || 0));
  if (!total) return '即将到期';
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return `${h} 小时 ${m} 分`;
  if (m) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

function fmtTime(value) {
  if (!value) return '-';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString();
}

// LiveCountdown 以「绝对截止时间」为基准每秒自减。
// 只让本组件按秒重渲染，避免整张账号表每秒重绘。
function LiveCountdown({ deadline, fallbackSec = 0 }) {
  const compute = () => (deadline
    ? Math.max(0, Math.round((deadline - Date.now()) / 1000))
    : Math.max(0, Math.round(fallbackSec)));
  const [remaining, setRemaining] = useState(compute);
  React.useEffect(() => {
    setRemaining(compute());
    if (!deadline) return undefined;
    const timer = setInterval(() => setRemaining(compute()), 1000);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deadline, fallbackSec]);
  return <span>{fmtDuration(remaining)}</span>;
}

// coolingPortrait 统一计算账号的冷却画像。
// 关键点：后端的 cool_remaining_sec **只反映 until**，而 cooling 是
// (until 生效 || breaker_until 生效)。纯熔断账号（until 已过期、breaker 仍生效）
// 若只读剩余秒数会误显示"即将到期"，因此这里用两个绝对截止时间取较晚者。
export function coolingPortrait(record) {
  const now = Date.now();
  const untilMs = parseDeadline(record.until);
  const breakerMs = parseDeadline(record.breaker_until);
  const untilActive = untilMs > now;
  const breakerActive = breakerMs > now;
  const deadline = Math.max(untilActive ? untilMs : 0, breakerActive ? breakerMs : 0);
  return {
    untilActive,
    breakerActive,
    deadline: deadline || 0,
    reason: record.reason || '',
    kind: COOL_KIND_LABEL[record.cool_kind] || record.cool_kind || '',
  };
}

/**
 * AccountList 账号列表页（hash 路由 #/pool）。
 *
 * 从仪表盘"账号池"标签页拆出来的独立页面：账号池统计卡、筛选（搜索/
 * 状态/区域）与账号状态表（含启用/禁用、清冷却、签到、保活、删除）。
 *
 * 数据流：data（/status 聚合 + accounts 明细）、creditRefreshing、
 * refresh、refreshCredits 由父级 Console 传入 —— 账号操作后调用 refresh()
 * 让仪表盘与本页同时反映最新状态。api() 亦由父级传入（带鉴权头）。
 */
export default function AccountList({ api, data, refresh, refreshCredits, creditRefreshing }) {
  const { message, modal } = App.useApp();
  const [accountAction, setAccountAction] = useState('');
  const [accountSearch, setAccountSearch] = useState('');
  const [accountStatusFilter, setAccountStatusFilter] = useState('all');
  const [accountRegionFilter, setAccountRegionFilter] = useState('all');
  // 分组筛选（'all' = 不过滤）。
  const [accountGroupFilter, setAccountGroupFilter] = useState('all');
  // 分组功能：groups 列表来自 /admin/groups；每账号归属来自 /status 的
  // account_groups 快照（一次拉齐，不逐账号 GET）。老后端两个字段都缺时
  // groupNames 为 null，分组列与按钮整体隐藏。
  const [groupNames, setGroupNames] = useState(null);

  useEffect(() => {
    if (groupNames !== null || !data.accounts?.length) return;
    api('/admin/groups')
      .then(body => setGroupNames((body.groups || []).map(g => g.name)))
      .catch(() => setGroupNames(null));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data.accounts?.length > 0]);

  // 每账号分组：data.account_groups 里没有的 uid = 未登记 = default 语义。
  const accountGroups = data.account_groups || {};

  // editAccountGroups 打开分组编辑弹窗（多选）。未登记账号初始值 = default。
  const editAccountGroups = record => {
    const uid = record.uid || '';
    const current = accountGroups[uid];
    let selected = current && current.length ? [...current] : ['default'];
    modal.confirm({
      title: `设置账号 ${record.nickname || uid.slice(0, 12)} 的分组`,
      icon: null,
      content: (
        <div style={{ marginTop: 12 }}>
          <Text type="secondary">账号可属于多个分组；分组密钥只能用对应分组里的账号。</Text>
          <Select
            mode="multiple"
            defaultValue={selected}
            style={{ width: '100%', marginTop: 8 }}
            onChange={value => { selected = value; }}
            options={(groupNames || []).map(g => ({ value: g, label: g }))}
          />
        </div>
      ),
      okText: '保存',
      cancelText: '取消',
      onOk: async () => {
        try {
          await api(`/admin/accounts/${encodeURIComponent(uid)}/groups`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ groups: selected.length ? selected : ['default'] }),
          });
          message.success('分组已保存');
          await refresh(); // /status 的 account_groups 快照随之更新
        } catch (error) {
          message.error(error.message);
          throw error;
        }
      },
    });
  };

  const runAccountAction = async (record, action) => {
    const uid = record.uid || '';
    const isDelete = action === 'delete';
    const isEnable = action === 'enable';
    setAccountAction(`${uid}:${action}`);
    try {
      const path = isDelete ? `/admin/account/${encodeURIComponent(uid)}` : `/admin/account/${encodeURIComponent(uid)}/${action}`;
      await api(path, { method: isDelete ? 'DELETE' : 'POST' });
      const sessionWarning = isEnable && /12153|session dead/i.test(record.reason || '')
        ? '账号已启用，但凭证曾失效；如果再次被禁用，请重新授权登录。'
        : isDelete ? '账号已删除'
          : isEnable ? '账号已手动启用'
            : action === 'clear-cooldown' ? '冷却与熔断已清除'
              : '账号已禁用';
      message.success(sessionWarning);
      await refresh();
    } catch (error) {
      message.error(error.message);
    } finally {
      setAccountAction('');
    }
  };

  // runUpstreamAction 处理签到/保活：两者共用一个后端口径 —— HTTP 200 表示
  // 请求已送达，真正的结果由 ok/detail 表达。上游不可达时 ok=false 但接口是通的，
  // 因此这里按 ok 决定提示颜色，而不是按异常处理。
  const runUpstreamAction = async (record, action) => {
    const uid = record.uid || '';
    const label = action === 'checkin' ? '签到' : '保活';
    setAccountAction(`${uid}:${action}`);
    try {
      const result = await api(`/admin/account/${encodeURIComponent(uid)}/${action}`, { method: 'POST' });
      if (result.ok) message.success(`${label}成功`);
      else message.warning(`${label}未完成：${result.detail || '上游未返回成功'}`);
      await refresh();
    } catch (error) {
      message.error(`${label}失败：${error.message}`);
    } finally {
      setAccountAction('');
    }
  };

  const accountActionHandler = (record, action) => {
    // 启用是可逆操作，直接执行并立即反馈；删除和禁用仍需二次确认。
    if (action === 'enable' || action === 'clear-cooldown') {
      runAccountAction(record, action);
      return;
    }
    if (action === 'checkin' || action === 'keepalive') {
      runUpstreamAction(record, action);
      return;
    }
    const uid = record.uid || '';
    const isDelete = action === 'delete';
    modal.confirm({
      title: isDelete ? '确认删除账号？' : '确认禁用账号？',
      content: isDelete
        ? `账号 ${uid.slice(0, 12)} 将从账号池和本地授权文件中删除，删除后需要重新登录才能恢复。`
        : `账号 ${uid.slice(0, 12)} 将停止接收新请求，之后可以手动启用。`,
      okText: isDelete ? '删除' : '禁用',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: () => runAccountAction(record, action),
    });
  };

  // filteredAccounts 支持按昵称/UID 搜索 + 状态/区域/分组筛选，与表格排序组合使用。
  // 分组筛选语义：多归属账号属于任一所选分组即命中（OR）。
  const filteredAccounts = useMemo(() => {
    const query = accountSearch.trim().toLowerCase();
    return (data.accounts || []).filter(record => {
      const portrait = coolingPortrait(record);
      const modelCooling = Object.keys(record.model_cooldowns || {}).length > 0;
      const matchesQuery = !query || [record.nickname, record.uid, record.reason]
        .some(value => String(value || '').toLowerCase().includes(query));
      const matchesRegion = accountRegionFilter === 'all' || (record.region || 'cn') === accountRegionFilter;
      let matchesGroup = true;
      if (accountGroupFilter !== 'all') {
        const gs = accountGroups[record.uid];
        const shown = gs && gs.length ? gs : ['default'];
        matchesGroup = shown.includes(accountGroupFilter);
      }
      let matchesStatus = true;
      if (accountStatusFilter === 'healthy') {
        matchesStatus = !record.disabled && !portrait.untilActive && !portrait.breakerActive && !modelCooling;
      } else if (accountStatusFilter === 'cooling') {
        matchesStatus = !record.disabled && (portrait.untilActive || portrait.breakerActive);
      } else if (accountStatusFilter === 'disabled') {
        matchesStatus = !!record.disabled;
      }
      return matchesQuery && matchesRegion && matchesGroup && matchesStatus;
    });
  }, [data.accounts, accountGroups, accountSearch, accountStatusFilter, accountRegionFilter, accountGroupFilter]);

  const columns = [
    {
      title: '账号', dataIndex: 'nickname',
      render: (value, record) => (
        <Space direction="vertical" size={0}>
          <Text strong>{value || '未命名'}</Text>
          <Text type="secondary" code>{record.uid?.slice(0, 12)}</Text>
        </Space>
      ),
    },
    // 分组列：分组存储可用时展示（默认隐藏于老后端）。
    // 未登记的账号显示 default（后端语义：未登记 = default 归属）。
    ...(groupNames ? [{
      title: '分组',
      key: 'groups',
      render: (_, record) => {
        const gs = accountGroups[record.uid];
        const shown = gs && gs.length ? gs : ['default'];
        return <Space size={2} wrap>{shown.map(g => <Tag key={g} color={g === 'default' ? 'blue' : 'purple'} style={{ marginInlineEnd: 0 }}>{g}</Tag>)}</Space>;
      },
    }] : []),
    {
      title: '区域',
      dataIndex: 'region',
      render: value => <Tag color={value === 'global' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : '中国区'}</Tag>,
    },
    {
      title: '状态',
      filters: [
        { text: '可用', value: 'healthy' },
        { text: '冷却', value: 'cooling' },
        { text: '模型限流', value: 'model_cooling' },
        { text: '禁用', value: 'disabled' },
      ],
      onFilter: (value, record) => {
        const portrait = coolingPortrait(record);
        const modelCooling = Object.keys(record.model_cooldowns || {}).length > 0;
        if (value === 'disabled') return !!record.disabled;
        if (value === 'cooling') return !record.disabled && portrait.untilActive;
        if (value === 'model_cooling') return !record.disabled && modelCooling;
        return !record.disabled && !portrait.untilActive && !portrait.breakerActive && !modelCooling;
      },
      render: (_, record) => {
        const portrait = coolingPortrait(record);
        const modelLimits = Object.entries(record.model_cooldowns || {});
        const modelLimitText = modelLimits.map(([model, limit]) => {
          const modelUntil = parseDeadline(limit?.until);
          const recovery = modelUntil
            ? `恢复：${new Date(modelUntil).toLocaleString()}`
            : (Number(limit?.remaining_sec) > 0 ? `${limit.remaining_sec} 秒后重试` : '等待上游恢复');
          return `${model}：${limit?.reason || '上游限流'}（${recovery}）`;
        });
        const modelCooling = modelLimits.length > 0;
        // 纯熔断账号（until 已过期、breaker 仍生效）单独标注，避免与普通冷却混淆。
        const pureBreaker = !portrait.untilActive && portrait.breakerActive && !record.disabled;
        const label = record.disabled ? '禁用'
          : pureBreaker ? '熔断退避'
            : portrait.untilActive ? (record.cool_kind === 'rate_limit' ? '上游限流' : '冷却')
              : modelCooling ? '模型限流' : '可用';
        const color = record.disabled ? 'red'
          : pureBreaker ? 'volcano'
            : portrait.untilActive ? 'orange'
              : modelCooling ? 'gold' : 'green';
        const title = [
          portrait.reason && `原因：${portrait.reason}`,
          portrait.kind && `类型：${portrait.kind}`,
          record.breaker_fails > 0 && `熔断计数：${record.breaker_fails}`,
          modelLimitText.length > 0 && `模型限流：\n${modelLimitText.join('\n')}`,
        ].filter(Boolean).join('\n');
        return <Tag color={color} title={title || undefined}>{label}</Tag>;
      },
    },
    {
      title: '原因 / 剩余',
      width: 190,
      // 冷却是瞬时状态，这里按秒自减展示，而不是等 30 秒轮询才更新。
      render: (_, record) => {
        const portrait = coolingPortrait(record);
        if (record.disabled) {
          return record.reason
            ? <Text type="danger" title={record.reason}>{record.reason}</Text>
            : <Text type="secondary">人工禁用</Text>;
        }
        if (!portrait.untilActive && !portrait.breakerActive) {
          return record.reason ? <Text type="secondary">{record.reason}</Text> : <Text type="secondary">-</Text>;
        }
        return (
          <Space direction="vertical" size={0}>
            <Text title={portrait.reason || undefined}>
              {portrait.untilActive ? (portrait.kind || '冷却中') : '熔断退避'}
            </Text>
            <Text type="secondary">
              剩余 <LiveCountdown deadline={portrait.deadline} fallbackSec={record.cool_remaining_sec} />
            </Text>
          </Space>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      fixed: 'right',
      width: 300,
      render: (_, record) => {
        const uid = record.uid || '';
        const busy = accountAction.startsWith(`${uid}:`);
        const portrait = coolingPortrait(record);
        const cooling = portrait.untilActive || portrait.breakerActive;
        return (
          <Space size={4} wrap>
            <Button
              size="small"
              type="link"
              icon={record.disabled ? <UnlockOutlined /> : <StopOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, record.disabled ? 'enable' : 'disable')}
            >
              {record.disabled ? '手动启用' : '禁用'}
            </Button>
            <Tooltip title="清除冷却与熔断并立刻恢复参与选号（不改动禁用状态）">
              <Button
                size="small"
                type="link"
                icon={<ThunderboltOutlined />}
                loading={busy}
                disabled={!cooling}
                onClick={() => accountActionHandler(record, 'clear-cooldown')}
              >清冷却</Button>
            </Tooltip>
            {groupNames && (
              <Button
                size="small"
                type="link"
                icon={<ApartmentOutlined />}
                onClick={() => editAccountGroups(record)}
              >分组</Button>
            )}
            <Button
              size="small"
              type="link"
              icon={<CheckCircleOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'checkin')}
            >签到</Button>
            <Button
              size="small"
              type="link"
              icon={<ReloadOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'keepalive')}
            >保活</Button>
            <Button
              size="small"
              type="link"
              danger
              icon={<DeleteOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'delete')}
            >删除</Button>
          </Space>
        );
      },
    },
    { title: '可用积分', dataIndex: 'credits', sorter: (a, b) => (a.credits || 0) - (b.credits || 0), render: fmt },
    {
      title: '周期积分',
      render: (_, record) => record.cycle_capacity_size
        ? `${fmt(record.cycle_capacity_used)} / ${fmt(record.cycle_capacity_size)}`
        : '-',
    },
    {
      title: '累计积分',
      render: (_, record) => record.capacity_size
        ? `${fmt(record.capacity_used)} / ${fmt(record.capacity_size)}`
        : '-',
    },
    {
      title: '积分更新', dataIndex: 'credit_updated_at',
      sorter: (a, b) => (a.credit_updated_at || 0) - (b.credit_updated_at || 0),
      render: value => value ? new Date(value * 1000).toLocaleString() : '-',
    },
    {
      title: 'token',
      width: 130,
      // 凭证到期时间：后端透出 Unix 秒。剩余不足 10 分钟标红，便于提前重新授权。
      sorter: (a, b) => (a.token_expires_at || 0) - (b.token_expires_at || 0),
      render: (_, record) => {
        const label = tokenExpiryLabel(record.token_expires_at);
        if (!label) return <Text type="secondary">-</Text>;
        return <Text type={label.danger ? 'danger' : 'secondary'} title={fmtTime(record.token_expires_at * 1000)}>{label.text}</Text>;
      },
    },
    {
      title: '成功率',
      sorter: (a, b) => {
        const ratio = record => {
          const total = (record.success_count || 0) + (record.err_total || 0);
          return total ? record.success_count / total : -1;
        };
        return ratio(a) - ratio(b);
      },
      render: (_, record) => {
        const total = (record.success_count || 0) + (record.err_total || 0);
        return total ? `${Math.round((record.success_count / total) * 100)}%` : '-';
      },
    },
    {
      title: '在途', dataIndex: 'in_flight',
      sorter: (a, b) => (a.in_flight || 0) - (b.in_flight || 0),
    },
  ];

  // 统计卡与筛选同步：对 filteredAccounts 实时计算（而非全池 data.*），
  // 筛选变化即时反映；全池口径在副标题保留对照。
  const filteredTotal = filteredAccounts.length;
  const filteredHealthy = filteredAccounts.filter(r => !r.disabled && !coolingPortrait(r).untilActive
    && !coolingPortrait(r).breakerActive && Object.keys(r.model_cooldowns || {}).length === 0).length;
  const filteredCooling = filteredAccounts.filter(r => !r.disabled && (coolingPortrait(r).untilActive || coolingPortrait(r).breakerActive)).length;
  const filteredDisabled = filteredAccounts.filter(r => !!r.disabled).length;
  const filteredCredits = filteredAccounts.reduce((sum, r) => sum + Number(r.credits || 0), 0);

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', gap: 12 }}>
        <Card><Statistic title={`账号总数${filteredTotal !== (data.total || 0) ? `（筛选后）` : ''}`} value={filteredTotal} suffix={filteredTotal !== (data.total || 0) ? <Text type="secondary" style={{ fontSize: 14 }}>/ {data.total || 0}</Text> : undefined} /></Card>
        <Card><Statistic title={`可用${filteredHealthy !== (data.healthy || 0) ? '（筛选后）' : ''}`} value={filteredHealthy} valueStyle={{ color: '#389e0d' }} /></Card>
        <Card><Statistic title={`冷却中${filteredCooling !== (data.cooling || 0) ? '（筛选后）' : ''}`} value={filteredCooling} valueStyle={{ color: '#d46b08' }} /></Card>
        <Card><Statistic title={`已禁用${filteredDisabled !== (data.disabled || 0) ? '（筛选后）' : ''}`} value={filteredDisabled} valueStyle={{ color: '#cf1322' }} /></Card>
        <Card><Statistic title="在途占满" value={data.in_flight_full || 0} /></Card>
        <Card><Statistic title="粘性会话" value={data.sticky_sessions || 0} /></Card>
        <Card><Statistic title={`剩余总余额${filteredTotal !== (data.total || 0) ? '（筛选后）' : ''}`} value={fmt(filteredCredits)} valueStyle={{ color: '#1677ff' }} /></Card>
      </div>
      <Card title="账号筛选">
        <Space wrap style={{ width: '100%' }}>
          <Input allowClear value={accountSearch} onChange={event => setAccountSearch(event.target.value)} placeholder="搜索昵称、UID 或冷却原因" style={{ minWidth: 260 }} />
          <Select value={accountStatusFilter} onChange={setAccountStatusFilter} style={{ width: 140 }} options={[
            { value: 'all', label: '全部状态' },
            { value: 'healthy', label: '可用' },
            { value: 'cooling', label: '冷却/熔断' },
            { value: 'disabled', label: '已禁用' },
          ]} />
          <Select value={accountRegionFilter} onChange={setAccountRegionFilter} style={{ width: 140 }} options={[
            { value: 'all', label: '全部区域' },
            { value: 'cn', label: '中国区' },
            { value: 'global', label: '海外版' },
          ]} />
          {groupNames && groupNames.length > 0 && (
            <Select value={accountGroupFilter} onChange={setAccountGroupFilter} style={{ width: 150 }} options={[
              { value: 'all', label: '全部分组' },
              ...groupNames.map(g => ({ value: g, label: `分组：${g}` })),
            ]} />
          )}
        </Space>
      </Card>
      <Card
        title="账号状态"
        extra={(
          <Space>
            <Text type="secondary">显示 {filteredAccounts.length} / {data.total || 0} 个账号</Text>
            {filteredTotal !== (data.total || 0) && (
              <Text type="secondary">筛选后剩余余额 {fmt(filteredCredits)}</Text>
            )}
            <Button size="small" icon={<ReloadOutlined />} loading={creditRefreshing} onClick={refreshCredits}>刷新上游积分</Button>
          </Space>
        )}
      >
        <Table
          rowKey="uid"
          columns={columns}
          dataSource={filteredAccounts}
          pagination={false}
          scroll={{ x: 1560 }}
          locale={{ emptyText: (data.total ? '暂无匹配的账号' : '账号池为空') }}
        />
      </Card>
    </Space>
  );
}
