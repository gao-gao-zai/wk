import React, { useEffect, useMemo, useState } from 'react';
import {
  Button, Card, Empty, Input, InputNumber, Select, Space, Statistic, Table, Tag, Typography,
} from 'antd';
import { ReloadOutlined, SearchOutlined } from '@ant-design/icons';

const { Text } = Typography;
const fmt = value => Number(value || 0).toLocaleString();
const fmtCredits = value => Number(value || 0).toFixed(4);
const fmtMillis = value => (Number(value) > 0 ? `${(Number(value) / 1000).toFixed(2)}s` : '-');

// WINDOW_PRESETS 快捷时间窗。absolute 标记的"全部"不传 since，由后端
// 返回库内所有日志（request_logs 上限 10000 条，本身有界）。
const WINDOW_PRESETS = [
  { key: '1h', label: '近 1 小时', hours: 1 },
  { key: '6h', label: '近 6 小时', hours: 6 },
  { key: '24h', label: '近 24 小时', hours: 24 },
  { key: '7d', label: '近 7 天', hours: 24 * 7 },
  { key: '30d', label: '近 30 天', hours: 24 * 30 },
];

/**
 * RequestLogs 请求日志页（独立页面，hash 路由 #/requests）。
 *
 * 与仪表盘的"最近请求"不同，这里做的是可检索的日志视图：
 *  - 筛选全部下推到后端（GET /requests?since=…&model=…），而不是把 200
 *    条近期记录拉回来在内存里过滤——时间窗拉长后浏览器端过滤既慢又不完整。
 *  - 筛选维度对应用户的排查路径：时间窗、模型、成功与否、状态码/错误码、
 *    端点、账号、区域（版本）、首字耗时区间、请求 ID。
 *  - 顶部统计卡来自 include=summary 的全量聚合（不受 limit 截断），
 *    看的是"窗口内总共发生了什么"，而表格只是窗口内最新的一页。
 *
 * 兼容性：老版本后端没有 since/model 等参数与 summary 字段——筛选参数
 * 会被忽略、summary 缺失，页面退化为"最近日志 + 浏览器端过滤"，不报错。
 */
export default function RequestLogs({ api, models, accounts }) {
  // —— 筛选状态（全部是受控组件，URL 不回写，避免筛选词泄进历史记录）——
  const [windowKey, setWindowKey] = useState('24h');
  const [modelFilter, setModelFilter] = useState('all');
  const [statusFilter, setStatusFilter] = useState('all'); // all | success | failed | 精确码（数字字符串）
  const [routeFilter, setRouteFilter] = useState('all');
  const [accountFilter, setAccountFilter] = useState('all');
  const [regionFilter, setRegionFilter] = useState('all');
  const [ttfbMin, setTtfbMin] = useState(null); // 毫秒
  const [ttfbMax, setTtfbMax] = useState(null);
  const [idSearch, setIdSearch] = useState('');
  const [logs, setLogs] = useState([]);
  const [summary, setSummary] = useState(null);
  const [loading, setLoading] = useState(false);

  // knownRoutes / knownModels / knownAccounts 从数据里推导选项，避免手写
  // 白名单漂移；模型目录由父组件传入（/v1/models），比日志里的更全。
  const knownRoutes = useMemo(() => [...new Set(logs.map(r => r.route).filter(Boolean))].sort(), [logs]);

  const query = useMemo(() => {
    const params = new URLSearchParams();
    params.set('limit', '500');
    const preset = WINDOW_PRESETS.find(w => w.key === windowKey);
    if (preset) {
      params.set('since', Math.floor(Date.now() / 1000) - preset.hours * 3600);
    }
    if (modelFilter !== 'all') params.set('model', modelFilter);
    if (routeFilter !== 'all') params.set('route', routeFilter);
    if (accountFilter !== 'all') params.set('account', accountFilter);
    if (regionFilter !== 'all') params.set('region', regionFilter);
    if (statusFilter === 'success') params.set('success', '1');
    else if (statusFilter === 'failed') params.set('success', '0');
    else if (statusFilter !== 'all') params.set('status', statusFilter);
    if (ttfbMin) params.set('ttfb_min', String(Math.round(ttfbMin)));
    if (ttfbMax) params.set('ttfb_max', String(Math.round(ttfbMax)));
    const id = idSearch.trim();
    if (id) params.set('id', id);
    params.set('include', 'summary');
    return params.toString();
  }, [windowKey, modelFilter, routeFilter, accountFilter, regionFilter, statusFilter, ttfbMin, ttfbMax, idSearch]);

  const load = async () => {
    setLoading(true);
    try {
      const body = await api(`/requests?${query}`);
      setLogs(body.data || []);
      setSummary(body.summary || null);
    } finally {
      setLoading(false);
    }
  };

  // 筛选变化即重新查询；时间窗用的是查询时刻的绝对值，刷新页面会重新计算。
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query]);

  // 浏览器端只做兜底过滤：老后端不认识筛选参数时（全部记录返回），
  // 至少保证视图口径一致；新后端返回的本来就是过滤后的结果。
  const visibleLogs = useMemo(() => {
    const preset = WINDOW_PRESETS.find(w => w.key === windowKey);
    const sinceSec = preset ? Math.floor(Date.now() / 1000) - preset.hours * 3600 : 0;
    return logs.filter(record => {
      if (preset && Number(record.created_at || 0) < sinceSec) return false;
      if (modelFilter !== 'all' && record.model !== modelFilter) return false;
      if (routeFilter !== 'all' && record.route !== routeFilter) return false;
      if (accountFilter !== 'all' && record.account_uid !== accountFilter) return false;
      if (regionFilter !== 'all' && record.account_region !== regionFilter) return false;
      const success = Number(record.status) >= 200 && Number(record.status) < 300;
      if (statusFilter === 'success' && !success) return false;
      if (statusFilter === 'failed' && success) return false;
      if (statusFilter !== 'all' && statusFilter !== 'success' && statusFilter !== 'failed' && String(record.status) !== statusFilter) return false;
      const ttfb = Number(record.ttfb_millis || 0);
      if (ttfbMin && ttfb < ttfbMin) return false;
      if (ttfbMax && ttfb > ttfbMax) return false;
      const id = idSearch.trim();
      if (id && record.id !== id) return false;
      return true;
    });
  }, [logs, windowKey, modelFilter, routeFilter, accountFilter, regionFilter, statusFilter, ttfbMin, ttfbMax, idSearch]);

  // 汇总统计：优先后端 summary（全量聚合，不受 limit 截断）；
  // 后端不支持时退回当前页数据的本地聚合（口径标注"当前页"）。
  const stats = useMemo(() => {
    if (summary && Number(summary.requests) > 0) {
      return {
        source: 'agg',
        requests: summary.requests, successes: summary.successes, failures: summary.failures,
        inputTokens: summary.input_tokens, outputTokens: summary.output_tokens,
        totalTokens: summary.total_tokens, credits: summary.credits_consumed,
        avgTtfb: summary.ttfb_samples > 0 ? summary.ttfb_millis_sum / summary.ttfb_samples : 0,
        avgLatency: summary.requests > 0 ? summary.latency_millis_sum / summary.requests : 0,
      };
    }
    const requests = visibleLogs.length;
    const successes = visibleLogs.filter(r => r.status >= 200 && r.status < 300).length;
    const ttfbSamples = visibleLogs.filter(r => Number(r.ttfb_millis || 0) > 0);
    return {
      source: 'page',
      requests, successes, failures: requests - successes,
      inputTokens: visibleLogs.reduce((s, r) => s + Number(r.input_tokens || 0), 0),
      outputTokens: visibleLogs.reduce((s, r) => s + Number(r.output_tokens || 0), 0),
      totalTokens: visibleLogs.reduce((s, r) => s + Number(r.total_tokens || 0), 0),
      credits: visibleLogs.reduce((s, r) => s + Number(r.credits_consumed || 0), 0),
      avgTtfb: ttfbSamples.length ? ttfbSamples.reduce((s, r) => s + Number(r.ttfb_millis || 0), 0) / ttfbSamples.length : 0,
      avgLatency: requests ? visibleLogs.reduce((s, r) => s + Number(r.latency_millis || 0), 0) / requests : 0,
    };
  }, [summary, visibleLogs]);

  const modelOptions = useMemo(() => {
    const ids = new Set(models.map(m => m.id));
    logs.forEach(r => { if (r.model && r.model !== '-') ids.add(r.model); });
    return [{ value: 'all', label: '全部模型' }, ...[...ids].sort().map(id => ({ value: id, label: id }))];
  }, [models, logs]);

  const accountOptions = useMemo(() => {
    const seen = new Map();
    (accounts || []).forEach(a => seen.set(a.uid, a.nickname || a.uid?.slice(0, 12) || a.uid));
    logs.forEach(r => { if (r.account_uid && !seen.has(r.account_uid)) seen.set(r.account_uid, r.account_uid.slice(0, 12)); });
    return [{ value: 'all', label: '全部账号' }, ...[...seen.entries()].map(([uid, label]) => ({ value: uid, label }))];
  }, [accounts, logs]);

  const columns = [
    { title: '时间', dataIndex: 'created_at', width: 170, render: value => value ? new Date(value * 1000).toLocaleString() : '-' },
    { title: '请求 ID', dataIndex: 'id', width: 200, render: value => <Text code copyable={{ text: value || '' }}>{value || '-'}</Text> },
    { title: '端点', dataIndex: 'route', width: 170, render: value => <Text code>{value || '-'}</Text> },
    { title: '模型', dataIndex: 'model', render: value => <Text strong>{value || '-'}</Text> },
    { title: '模式', width: 80, render: (_, record) => <Tag color={record.passthrough ? 'purple' : 'blue'}>{record.passthrough ? '透传' : record.mode === 'stream' ? '流式' : record.mode === 'sync' ? '同步' : record.mode || '-'}</Tag> },
    {
      title: '状态', dataIndex: 'status', width: 90,
      render: value => <Tag color={value >= 200 && value < 300 ? 'green' : 'red'}>{value || '-'}</Tag>,
    },
    { title: '输入', dataIndex: 'input_tokens', width: 90, render: fmt },
    { title: '输出', dataIndex: 'output_tokens', width: 90, render: fmt },
    {
      title: '积分消耗', width: 110,
      render: (_, record) => (
        <Space direction="vertical" size={0}>
          <Text>{record.credit_source && record.credit_source !== 'unknown' ? fmtCredits(record.credits_consumed) : '-'}</Text>
          <Text type="secondary">{{ upstream: '上游', estimated: '估算', unknown: '未知' }[record.credit_source] || '未知'}</Text>
        </Space>
      ),
    },
    { title: '首 token', dataIndex: 'ttfb_millis', width: 100, render: value => value ? `${value}ms` : '-' },
    { title: '耗时', dataIndex: 'latency_millis', width: 100, render: value => value ? `${(value / 1000).toFixed(2)}s` : '-' },
    { title: '账号', dataIndex: 'account_uid', width: 140, render: value => value ? <Text code>{value.slice(0, 12)}</Text> : '-' },
    { title: '版本', dataIndex: 'account_region', width: 90, render: value => <Tag color={value === 'global' ? 'gold' : value === 'cn' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : value === 'cn' ? '国内版' : '未知'}</Tag> },
    {
      title: '错误', width: 140,
      render: (_, record) => record.error_code || record.error_message
        ? <Text type="danger" title={record.error_message || record.error_code}>{record.error_code || 'error'}</Text>
        : '-',
    },
  ];

  const hasFilter = modelFilter !== 'all' || statusFilter !== 'all' || routeFilter !== 'all'
    || accountFilter !== 'all' || regionFilter !== 'all' || ttfbMin || ttfbMax || idSearch.trim();

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(160px,1fr))', gap: 12 }}>
        <Card><Statistic title="请求数" value={fmt(stats.requests)} suffix={stats.source === 'page' ? '（当前页）' : ''} /></Card>
        <Card><Statistic title="成功 / 失败" value={`${fmt(stats.successes)} / ${fmt(stats.failures)}`} valueStyle={{ color: stats.failures > 0 ? '#cf1322' : '#389e0d' }} /></Card>
        <Card><Statistic title="输入 / 输出 token" value={`${fmt(stats.inputTokens)} / ${fmt(stats.outputTokens)}`} /></Card>
        <Card><Statistic title="总 token" value={fmt(stats.totalTokens)} /></Card>
        <Card><Statistic title="积分消耗" value={fmtCredits(stats.credits)} valueStyle={{ color: '#d46b08' }} /></Card>
        <Card><Statistic title="平均首 token" value={stats.avgTtfb ? `${(stats.avgTtfb / 1000).toFixed(2)}s` : '-'} /></Card>
        <Card><Statistic title="平均耗时" value={stats.avgLatency ? `${(stats.avgLatency / 1000).toFixed(2)}s` : '-'} /></Card>
      </div>

      <Card title="筛选" extra={<Text type="secondary">统计基于时间窗内全部匹配记录；表格最多展示 500 条</Text>}>
        <Space wrap style={{ width: '100%' }}>
          <Select value={windowKey} onChange={setWindowKey} style={{ width: 130 }} options={WINDOW_PRESETS.map(w => ({ value: w.key, label: w.label }))} />
          <Select value={modelFilter} onChange={setModelFilter} style={{ minWidth: 200 }} showSearch options={modelOptions} />
          <Select value={statusFilter} onChange={setStatusFilter} style={{ width: 150 }} options={[
            { value: 'all', label: '全部状态' },
            { value: 'success', label: '成功' },
            { value: 'failed', label: '失败' },
          ]} />
          <Select value={routeFilter} onChange={setRouteFilter} style={{ width: 190 }} options={[
            { value: 'all', label: '全部端点' },
            ...knownRoutes.map(route => ({ value: route, label: route })),
          ]} />
          <Select value={accountFilter} onChange={setAccountFilter} style={{ minWidth: 170 }} showSearch options={accountOptions} />
          <Select value={regionFilter} onChange={setRegionFilter} style={{ width: 130 }} options={[
            { value: 'all', label: '全部版本' },
            { value: 'cn', label: '国内版' },
            { value: 'global', label: '海外版' },
          ]} />
          <InputNumber min={0} placeholder="首字下限 ms" value={ttfbMin} onChange={setTtfbMin} style={{ width: 120 }} />
          <InputNumber min={0} placeholder="首字上限 ms" value={ttfbMax} onChange={setTtfbMax} style={{ width: 120 }} />
          <Input allowClear value={idSearch} onChange={e => setIdSearch(e.target.value)} placeholder="请求 ID 精确匹配" style={{ width: 220 }} prefix={<SearchOutlined />} />
          <Button icon={<ReloadOutlined />} loading={loading} onClick={load}>刷新</Button>
        </Space>
        <Space wrap style={{ marginTop: 8 }}>
          <Text type="secondary">状态码：</Text>
          {[200, 400, 401, 403, 429, 500, 502, 503].map(code => (
            <Tag.CheckableTag key={code} checked={statusFilter === String(code)} onChange={checked => setStatusFilter(checked ? String(code) : 'all')}>{code}</Tag.CheckableTag>
          ))}
          {hasFilter && (
            <Button type="link" size="small" onClick={() => {
              setModelFilter('all'); setStatusFilter('all'); setRouteFilter('all');
              setAccountFilter('all'); setRegionFilter('all'); setTtfbMin(null); setTtfbMax(null); setIdSearch('');
            }}>重置筛选</Button>
          )}
        </Space>
      </Card>

      <Card title="请求明细" extra={<Text type="secondary">显示 {visibleLogs.length} 条</Text>}>
        {visibleLogs.length === 0 && !loading
          ? <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="当前筛选条件下没有匹配的请求记录" />
          : (
            <Table
              rowKey={record => `${record.id}-${record.created_at}`}
              size="small"
              columns={columns}
              dataSource={visibleLogs}
              pagination={{ pageSize: 20, showSizeChanger: true, pageSizeOptions: [10, 20, 50] }}
              scroll={{ x: 1780 }}
              expandable={{
                expandedRowRender: record => (
                  <Space direction="vertical" size={4}>
                    <Text type="secondary">请求 ID：{record.id || '-'}</Text>
                    <Text type="secondary">请求上限：{record.requested_output_tokens ? fmt(record.requested_output_tokens) : '未设置'}；缓存读取：{fmt(record.cache_read_tokens)}；缓存创建：{fmt(record.cache_write_tokens)}；工具调用：{fmt(record.tool_calls)}</Text>
                    <Text type="secondary">模式：{record.passthrough ? '透传' : record.mode}；耗时：{fmtMillis(record.ttfb_millis)}（首字）/ {fmtMillis(record.latency_millis)}（总）</Text>
                    {(record.error_code || record.error_message) && <Text type="danger">{record.error_code || 'error'}：{record.error_message || '无错误详情'}</Text>}
                  </Space>
                ),
              }}
            />
          )}
      </Card>
    </Space>
  );
}
