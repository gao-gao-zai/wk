import React, { useMemo } from 'react';
import {
  Alert, Card, Empty, Table, Tag, Tooltip, Typography,
} from 'antd';
import { BarChartOutlined } from '@ant-design/icons';

const { Text } = Typography;

// 事件类型 → 标签色（和值班员日志的语义一致）。
const evTag = (ev) => {
  if (!ev) return null;
  if (ev.includes('新码')) return <Tag color="green">新码</Tag>;
  if (ev.includes('降价')) return <Tag color="blue">降价</Tag>;
  if (ev.includes('补货')) return <Tag color="geekblue">补货</Tag>;
  if (ev.includes('重新上架')) return <Tag color="purple">重新上架</Tag>;
  return <Tag>{ev}</Tag>;
};

const fmtDur = (sec) => {
  if (!sec || sec < 0) return '-';
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return s ? `${m}m${s}s` : `${m}m`;
};

/**
 * WatchHistoryPanel 监控加号成果面板。
 *
 * 数据全部来自 GET /admin/account/sms/haozhuma/watch 的 events/today
 * 字段（父组件 WatchCard 轮询好传入，这里纯展示，不自己发请求）：
 *   - 今日成果一览（触发/成功/花费/成功率）
 *   - 触发事件流水（倒序，最新在前）：每个码什么时候、因什么事件、
 *     什么价触发，加了几个号、真实花费、耗时、结果（差码冷却会标注）
 *   - 对接码消耗排行：哪个码贡献最多号、单价如何
 */
export default function WatchHistoryPanel({ watch }) {
  const events = watch?.events || [];
  const today = watch?.today || { triggers: 0, ok: 0, spent: 0, success_rate: 0 };
  const totals = {
    triggers: watch?.trigger_total ?? 0,
    ok: watch?.enrolled_total ?? 0,
    spent: watch?.spent_total ?? 0,
  };

  // 对接码消耗排行（从事件流水聚合）。
  const perUID = useMemo(() => {
    const m = new Map();
    for (const e of events) {
      const cur = m.get(e.uid) || { uid: e.uid, sid: e.sid, rounds: 0, ok: 0, cost: 0, lastPrice: e.price };
      cur.rounds += 1;
      cur.ok += e.ok || 0;
      cur.cost += e.cost || 0;
      cur.lastPrice = e.price;
      m.set(e.uid, cur);
    }
    return [...m.values()].sort((a, b) => b.ok - a.ok || b.cost - a.cost);
  }, [events]);

  const columns = [
    {
      title: '时间', dataIndex: 'time', key: 'time', width: 150,
      render: t => <Text style={{ fontSize: 12 }}>{t}</Text>,
    },
    { title: '项目', dataIndex: 'sid', key: 'sid', width: 80 },
    {
      title: '对接码', dataIndex: 'uid', key: 'uid', width: 170, ellipsis: true,
      render: u => <Text copyable={{ text: u }} style={{ fontSize: 12 }}>{u}</Text>,
    },
    { title: '事件', dataIndex: 'ev', key: 'ev', width: 110, render: evTag },
    {
      title: '单价', dataIndex: 'price', key: 'price', width: 80, align: 'right',
      render: p => `¥${Number(p).toFixed(2)}`,
    },
    {
      title: '成功/目标', key: 'okwant', width: 100, align: 'center',
      render: (_, e) => (
        <span>
          <Text strong style={{ color: e.ok > 0 ? '#389e0d' : '#cf1322' }}>{e.ok}</Text>
          <Text type="secondary"> / {e.want}</Text>
        </span>
      ),
    },
    {
      title: '花费', dataIndex: 'cost', key: 'cost', width: 80, align: 'right',
      render: c => (c > 0 ? `¥${Number(c).toFixed(2)}` : <Text type="secondary">¥0</Text>),
    },
    { title: '耗时', dataIndex: 'duration', key: 'duration', width: 70, align: 'right', render: fmtDur },
    {
      title: '结果', dataIndex: 'note', key: 'note', ellipsis: true,
      render: (n, e) => {
        if (e.ok > 0 && !n) return <Tag color="success">成功</Tag>;
        if (!n) return <Tag color="success">完成</Tag>;
        // 差码路径：0 成功 + 熔断原因 → 已自动冷却并移出账户。
        return (
          <Tooltip title={`${n}${e.ok === 0 ? '（已自动冷却 24h 并移出账户）' : ''}`}>
            <Tag color={e.ok === 0 ? 'error' : 'warning'}>{e.ok === 0 ? '差码·已冷却' : '部分成功'}</Tag>
          </Tooltip>
        );
      },
    },
  ];

  const uidCols = [
    { title: '对接码', dataIndex: 'uid', key: 'uid', ellipsis: true },
    { title: '轮数', dataIndex: 'rounds', key: 'rounds', width: 70, align: 'right' },
    { title: '成功加号', dataIndex: 'ok', key: 'ok', width: 90, align: 'right' },
    { title: '累计花费', key: 'cost', width: 90, align: 'right', render: r => `¥${r.cost.toFixed(2)}` },
    { title: '最近单价', key: 'lastPrice', width: 90, align: 'right', render: r => `¥${r.lastPrice.toFixed(2)}` },
  ];

  const stat = (label, value, sub) => (
    <div style={{ minWidth: 110 }}>
      <Text type="secondary" style={{ fontSize: 12, display: 'block' }}>{label}</Text>
      <Text strong style={{ fontSize: 22 }}>{value}</Text>
      {sub && <Text type="secondary" style={{ fontSize: 12, marginLeft: 6 }}>{sub}</Text>}
    </div>
  );

  return (
    <Card
      size="small"
      title={(<span><BarChartOutlined /> 加号成果</span>)}
    >
      {events.length === 0 ? (
        <Empty
          image={Empty.PRESENTED_IMAGE_SIMPLE}
          description={(
            <Text type="secondary">
              还没有触发记录。值班员发现「新出现的低价码 / 降价进入心理价 / 补货」并成功加号后，
              这里会出现一条条成果记录。
            </Text>
          )}
        />
      ) : (
        <>
          {/* 今日成果一览 */}
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 24, marginBottom: 16 }}>
            {stat('今日触发', today.triggers, '轮')}
            {stat('今日成功加号', today.ok, '个')}
            {stat('今日花费', `¥${Number(today.spent).toFixed(2)}`)}
            {stat(
              '今日成功率',
              `${today.success_rate}%`,
              today.triggers > 0 ? `${today.triggers} 轮里成功 ${today.triggers * today.success_rate / 100 | 0} 轮` : '',
            )}
            {stat('累计', `${totals.ok} 个`, `¥${Number(totals.spent).toFixed(2)} · ${totals.triggers} 轮`)}
          </div>

          {/* 事件流水 */}
          <Table
            size="small"
            rowKey={(_, i) => i}
            columns={columns}
            dataSource={events}
            pagination={events.length > 20 ? { pageSize: 20, size: 'small', hideOnSinglePage: true } : false}
            scroll={{ x: 950 }}
          />

          {/* 码消耗排行 */}
          {perUID.length > 0 && (
            <div style={{ marginTop: 16 }}>
              <Text type="secondary" style={{ fontSize: 12 }}>对接码贡献排行（事件窗口内）</Text>
              <Table
                size="small"
                style={{ marginTop: 6 }}
                rowKey="uid"
                columns={uidCols}
                dataSource={perUID.slice(0, 10)}
                pagination={false}
              />
            </div>
          )}
        </>
      )}
      {events.length === 0 && watch?.trigger_total > 0 && (
        <Alert
          type="info"
          showIcon
          style={{ marginTop: 12 }}
          message={`历史累计触发 ${watch.trigger_total} 轮（流水从本版本开始记录，重启后保留最近 200 条）`}
        />
      )}
    </Card>
  );
}
