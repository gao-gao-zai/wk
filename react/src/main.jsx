import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Alert, App as AntApp, Button, Card, ConfigProvider, Empty, Form, Input, InputNumber, Layout, Menu, Modal,
  Select, Space, Statistic, Switch, Table, Tag, Tooltip, Typography, message,
} from 'antd';
import {
  ApiOutlined, ApartmentOutlined, CheckCircleOutlined, DashboardOutlined, FileSearchOutlined, GiftOutlined,
  GlobalOutlined, KeyOutlined, ReloadOutlined, SendOutlined, SettingOutlined, TeamOutlined,
  ThunderboltOutlined, CloudServerOutlined, UserAddOutlined,
} from '@ant-design/icons';
import 'antd/dist/reset.css';
import './theme.css';
import AutoEnrollPage from './pages/AutoEnroll';
import GroupsAndKeysPage from './pages/GroupsAndKeys';
import ProxyPoolPage from './pages/ProxyPool';
import ReqProxyPage from './pages/ReqProxy';
import RequestLogsPage from './pages/RequestLogs';
import AccountSettingsPage from './pages/AccountSettings';
import AccountListPage from './pages/AccountList';
import GrowthTasksPage from './pages/GrowthTasks';

const { Header, Sider, Content } = Layout;
const { Title, Text, Paragraph } = Typography;
const fmt = value => Number(value || 0).toLocaleString();
const fmtCredits = value => Number(value || 0).toFixed(4);
const parseHours = value => String(value || '').split(',').map(item => item.trim()).filter(Boolean).map(Number);
// API Key 只放在内存里，不写 Web Storage。
//
// 旧实现把它存进 sessionStorage（并从 localStorage 迁移），而 sessionStorage
// 并不是 XSS 缓解手段 —— 同源脚本照样读得到，只是窗口缩短到一个标签页的
// 生命周期。这个 key 对**所有** /admin/* 端点都是有效的 Bearer 凭据，
// 一旦被打包的第三方依赖或任何未来注入读到，等于整个网关被接管。
// 同时清掉历史遗留值，避免旧版本写下的 key 继续留在浏览器里。
['sessionStorage', 'localStorage'].forEach(store => {
  try {
    window[store].removeItem('wb2api-api-key');
  } catch {
    // 隐私模式等场景下 Web Storage 可能不可用；清理失败不应影响控制台加载。
  }
});

// 冷却原因文案：已随账号列表拆到 #/pool 独立页面 AccountList（COOL_KIND_LABEL
// 在那边各自维护，避免跨文件耦合）。

// 页面标识。用 hash 路由（#/auto-enroll）而不是给每页单独打包：
// 刷新能停在原页、地址可收藏转发，且不引入 react-router 依赖。
const SECTIONS = ['dashboard', 'pool', 'models', 'playground', 'requests', 'accounts', 'auto-enroll', 'proxy', 'reqproxy', 'groups', 'growth', 'settings'];

// sectionFromHash 读取地址栏里的页面标识；非法/缺失时回落到仪表盘。
function sectionFromHash() {
  const raw = String(window.location.hash || '').replace(/^#\/?/, '').trim();
  return SECTIONS.includes(raw) ? raw : 'dashboard';
}

// fmtTime 供仪表盘/请求表的时间列使用；账号表的时间列在 AccountList 里自带。
function fmtTime(value) {
  if (!value) return '-';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString();
}

function Sparkline({ values, color = '#356ae6', ariaLabel = '趋势图' }) {
  if (!values.length) return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="等待采样" />;
  const width = 220;
  const height = 52;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const points = values.map((value, index) => {
    const x = values.length === 1 ? width / 2 : (index / (values.length - 1)) * width;
    const y = height - ((value - min) / span) * (height - 8) - 4;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
  return <svg className="sparkline" viewBox={`0 0 ${width} ${height}`} role="img" aria-label={ariaLabel}><polyline fill="none" stroke={color} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" points={points} /></svg>;
}

function Console() {
  // antd v5 的静态 message / Modal 依赖 react-dom 的 render，而 React 19 已移除它
  // （react-dom 只导出 createRoot 于 react-dom/client）。静态调用因此静默失效，
  // 表现为"点了按钮没有任何反馈"。必须改用 App 上下文提供的实例。
  const { message } = AntApp.useApp();
  const [form] = Form.useForm();
  // apiKey 只保存在组件状态里（不落 Web Storage，见文件顶部说明）：
  // 刷新页面后需要重新输入，这是有意的取舍 —— 换取 key 不被同源脚本读取。
  const [apiKey, setApiKey] = useState('');
  const [locked, setLocked] = useState(false);
  const [activeSection, setActiveSection] = useState(() => sectionFromHash());
  const [data, setData] = useState({ accounts: [], metrics: {}, total: 0, healthy: 0, cooling: 0, disabled: 0 });
  const [models, setModels] = useState([]);
  const [requestLogs, setRequestLogs] = useState([]);
  // 仪表盘时间窗：指标卡与最近请求都按这个窗口统计，默认 24 小时。
  // 拉取时把 since 传给 /requests（带 include=summary），统计走全量聚合。
  const [dashboardWindow, setDashboardWindow] = useState('24h');
  const [dashboardSummary, setDashboardSummary] = useState(null);
  const [config, setConfig] = useState({
    checkin_hours: [9, 21], keepalive_hours: [22], region: 'cn',
    features: null, billing: null, upstream: null, max_request_body: null,
  });
  const [selectedModel, setSelectedModel] = useState('');
  const [promptText, setPromptText] = useState('你好，请简短介绍一下你自己。');
  const [answer, setAnswer] = useState('');
  const [loading, setLoading] = useState(false);
  const [stream, setStream] = useState(true);
  const [requestInfo, setRequestInfo] = useState(null);
  const [metricSamples, setMetricSamples] = useState([]);
  const [creditRefreshing, setCreditRefreshing] = useState(false);
  const [checkinRunning, setCheckinRunning] = useState(false);
  // （账号操作/筛选状态 accountAction、accountSearch、accountStatusFilter、
  // accountRegionFilter 已随账号列表拆到 #/pool 独立页面 AccountList。）
  const refreshController = useRef(null);
  const refreshSerial = useRef(0);
  const requestController = useRef(null);
  const requestSerial = useRef(0);

  const headers = useMemo(() => (apiKey ? { Authorization: `Bearer ${apiKey}` } : {}), [apiKey]);
  const api = async (path, options = {}) => {
    const response = await fetch(path, { ...options, headers: { ...headers, ...(options.headers || {}) } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      // 把后端的结构化错误字段带出来：retryable 决定是否保留当前短信会话，
      // existing/disabled 用于"该手机号已在号池中"的确认弹窗。
      const error = Error(body?.error?.message || body?.error || `${response.status}`);
      error.status = response.status;
      error.retryable = body?.retryable === true;
      error.reason = body?.reason || '';
      error.existing = body?.existing === true;
      error.existingUid = body?.uid || '';
      error.existingNickname = body?.nickname || '';
      error.existingDisabled = body?.disabled === true;
      throw error;
    }
    return body;
  };

  // DASHBOARD_WINDOWS 仪表盘时间窗选项。key 同时用于 /requests?since=… 换算。
  const windowSeconds = key => ({ '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800, '30d': 2592000 }[key] || 0);

  const refresh = async () => {
    const serial = ++refreshSerial.current;
    refreshController.current?.abort();
    const controller = new AbortController();
    refreshController.current = controller;
    // 仪表盘按所选时间窗拉请求日志（since 秒级时间戳），并请求全量聚合
    // summary —— 指标卡显示的是窗口内全部请求，不受 limit 截断。
    const since = windowSeconds(dashboardWindow);
    const requestQuery = `/requests?limit=200${since ? `&since=${Math.floor(Date.now() / 1000) - since}` : ''}&include=summary`;
    try {
      const [status, modelList, requestList] = await Promise.all([
        api('/status', { signal: controller.signal }),
        api('/v1/models', { signal: controller.signal }),
        api(requestQuery, { signal: controller.signal }).catch(() => ({ data: [] })),
      ]);
      if (serial !== refreshSerial.current) return;
      setData(status);
      setModels(modelList.data || []);
      setRequestLogs(requestList.data || []);
      setDashboardSummary(requestList.summary || null);
      setSelectedModel(current => current || modelList.data?.[0]?.id || '');
      const metrics = status.metrics || {};
      const inputTokens = Number(metrics.input_tokens || 0);
      const cacheRate = inputTokens ? (Number(metrics.cache_read_tokens || 0) / inputTokens) * 100 : 0;
      setMetricSamples(current => [...current, { at: Date.now(), cacheRate, avgTTFB: Number(metrics.avg_ttfb_ms || 0) }].slice(-24));
      setLocked(false);
    } catch (error) {
      if (error.name === 'AbortError') return;
      // 401 的错误文案里带 invalid / credential / 密钥 等；宽松匹配避免后端
      // 文案微调后这里静默失效（锁死在解锁弹窗之外）。
      if (/invalid|credential|unauthorized|密钥/i.test(error.message)) setLocked(true);
      return;
    }
    try {
      const currentConfig = await api('/admin/config', { signal: controller.signal });
      if (serial !== refreshSerial.current) return;
      // features/billing/upstream/max_request_body 是运行时可变配置（老后端没有
      // 这些字段时保持 null，管理设置页据此隐藏对应区域，避免误导）。
      setConfig(current => ({
        ...current,
        ...(currentConfig.schedule || {}),
        region: currentConfig.region || current.region || 'cn',
        features: currentConfig.features || current.features,
        billing: currentConfig.billing || current.billing,
        upstream: currentConfig.upstream || current.upstream,
        sms: currentConfig.sms || current.sms,
        request_log_retention: currentConfig.request_log_retention || current.request_log_retention,
        max_request_body: currentConfig.max_request_body || current.max_request_body,
      }));
    } catch (error) {
      if (error.name === 'AbortError') return;
      // Dashboard data can still refresh when configuration is unavailable.
    }
  };

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 30000);
    return () => clearInterval(timer);
  }, [apiKey, dashboardWindow]);

  useEffect(() => {
    form.setFieldsValue({
      checkin: (config.checkin_hours || []).join(','),
      keepalive: (config.keepalive_hours || []).join(','),
      travel: (config.travel_hours || []).join(','),
      activity: (config.activity_hours || []).join(','),
      blackcat: (config.blackcat_hours || []).join(','),
    });
  }, [config, form]);

  // hash 路由：切换页面时写回地址栏；浏览器前进/后退（hashchange）时同步
  // 回 state。两处都必要——只写不同步会让后退键失效。
  useEffect(() => {
    const next = `#/${activeSection}`;
    if (window.location.hash !== next) window.location.hash = next;
  }, [activeSection]);

  useEffect(() => {
    const onHashChange = () => setActiveSection(sectionFromHash());
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, []);

  useEffect(() => () => {
    refreshController.current?.abort();
    requestController.current?.abort();
  }, []);

  const unlock = async () => {
    // 密钥解锁：输入管理员密钥（config api_key）→ 后端校验后发会话 cookie。
    // 密码通道已下线（多密钥体系下管理员密钥就是控制台凭据）；分组密钥
    // 不能解锁控制台——后端只认管理员。
    if (!apiKey.trim()) {
      message.error('请输入管理员密钥');
      return;
    }
    try {
      const response = await fetch('/admin/unlock', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: apiKey.trim() }),
      });
      if (!response.ok) {
        const body = await response.json();
        throw Error(body.error || '密钥错误');
      }
      setLocked(false);
      await refresh();
      message.success('控制台已解锁');
    } catch (error) {
      message.error(error.message);
    }
  };

  const saveConfig = async values => {
    try {
      const nextConfig = {
        checkin_hours: parseHours(values.checkin),
        keepalive_hours: parseHours(values.keepalive),
      };
      const invalid = Object.values(nextConfig).some(hours => (
        !hours.length || hours.some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23)
      ));
      if (invalid) {
        message.error('每项至少填写一个 0-23 的整数小时');
        return;
      }
      // 新排程字段（可选填写）：空值不上送，保留 config.json 现值（增量补丁语义）。
      if (values.travel !== undefined && String(values.travel || '').trim()) nextConfig.travel_hours = parseHours(values.travel);
      if (values.activity !== undefined && String(values.activity || '').trim()) nextConfig.activity_hours = parseHours(values.activity);
      if (values.blackcat !== undefined && String(values.blackcat || '').trim()) nextConfig.blackcat_hours = parseHours(values.blackcat);
      for (const key of ['travel_hours', 'activity_hours', 'blackcat_hours']) {
        if (nextConfig[key] && (nextConfig[key].some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23))) {
          message.error('小时必须在 0-23 之间');
          return;
        }
      }
      // 排程开关（checkbox 不勾 = 不上送，保留现值；勾选状态由 Switch 单独保存）。
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(nextConfig),
      });
      setConfig(current => ({ ...current, ...(result.schedule || nextConfig) }));
      message.success(result.restart_required ? '配置已保存，重启后生效' : '配置已保存并即时生效');
    } catch (error) {
      message.error(error.message);
    }
  };

  // saveScheduleToggle 保存单个排程开关（即时生效）。
  const saveScheduleToggle = async (key, checked) => {
    try {
      const body = {
        checkin_hours: config.checkin_hours || [9],
        keepalive_hours: config.keepalive_hours || [22],
        [key]: checked,
      };
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      });
      setConfig(current => ({ ...current, ...(result.schedule || { [key]: checked }) }));
      message.success('排程开关已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveFeatures 保存特性开关：即时生效（后端同时落盘 config.json 并推给
  // 运行时组件，无需重启）。schedule 字段是必填校验项，这里回传当前值。
  const saveFeatures = async features => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          features,
        }),
      });
      setConfig(current => ({ ...current, features: { ...(current.features || {}), ...features } }));
      message.success('特性开关已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveBilling 保存积分估算费率：即时生效。留空视为 0（不估算）。
  const saveBilling = async billing => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          billing,
        }),
      });
      setConfig(current => ({ ...current, billing: { ...(current.billing || {}), ...billing } }));
      message.success('费率已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveUpstreamTimeouts 保存上游超时（秒）：即时生效（已在途请求不受影响）。
  const saveUpstreamTimeouts = async upstream => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          upstream,
        }),
      });
      setConfig(current => ({ ...current, upstream: { ...(current.upstream || {}), ...upstream } }));
      message.success('上游超时已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveHaozhumaSid 保存豪猪项目 ID：即时生效（新取号立刻用新项目；
  // 在途号码按旧项目自然收尾）。schedule 是必填校验项，回传当前值。
  const saveHaozhumaSid = async sid => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          sms: { haozhuma: { sid } },
        }),
      });
      setConfig(current => ({ ...current, sms: { haozhuma: { ...(current.sms?.haozhuma || {}), sid } } }));
      message.success(result?.updated?.haozhuma_sid_restart_required ? '项目 ID 已保存，重启后生效' : '豪猪项目 ID 已切换，新取号立即生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveRequestLogRetention 保存请求日志保留策略（天数 + 条数双条件）：
  // 即时生效（下一次修剪按新值执行）。0 = 对应条件不限。
  const saveRequestLogRetention = async retention => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          request_log_retention: retention,
        }),
      });
      setConfig(current => ({ ...current, request_log_retention: retention }));
      message.success(result?.updated?.request_log_retention_restart_required ? '保留策略已保存，重启后生效' : '保留策略已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  // saveMaxRequestBody 保存请求体大小上限（MiB）：即时生效（下一条
  // chat/responses 请求按新上限判定，已在途请求不受影响）。
  const saveMaxRequestBody = async mib => {
    try {
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          checkin_hours: config.checkin_hours || [9],
          keepalive_hours: config.keepalive_hours || [22],
          max_request_body: { mib },
        }),
      });
      setConfig(current => ({
        ...current,
        max_request_body: { ...(current.max_request_body || {}), mib },
      }));
      message.success('请求体大小限制已保存并即时生效');
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    }
  };

  const refreshCredits = async () => {
    setCreditRefreshing(true);
    try {
      const result = await api('/admin/credits/refresh', { method: 'POST' });
      message.success(result.message || '上游积分刷新已启动');
      window.setTimeout(refresh, 2500);
    } catch (error) {
      message.error(error.message);
    } finally {
      setCreditRefreshing(false);
    }
  };

  const runCheckinAll = async () => {
    setCheckinRunning(true);
    try {
      const result = await api('/admin/checkin', { method: 'POST' });
      message.success(result.message || '签到任务已启动');
      window.setTimeout(refresh, 2500);
    } catch (error) {
      message.error(error.message);
    } finally {
      setCheckinRunning(false);
    }
  };

  const send = async () => {
    const serial = ++requestSerial.current;
    requestController.current?.abort();
    const controller = new AbortController();
    requestController.current = controller;
    const startedAt = performance.now();
    let firstTokenAt = 0;
    setLoading(true);
    setAnswer('');
    setRequestInfo({ state: 'running', duration: 0, ttft: 0, usage: null });
    try {
      const response = await fetch('/v1/responses', {
        method: 'POST', signal: controller.signal,
        headers: { 'Content-Type': 'application/json', ...headers },
        body: JSON.stringify({ model: selectedModel, input: promptText, stream }),
      });
      if (!response.ok) {
        const errorBody = await response.json().catch(() => ({}));
        throw Error(errorBody?.error?.message || errorBody?.error || `请求失败 (${response.status})`);
      }
      let finalResponse = null;
      if (!stream) {
        finalResponse = await response.json();
        setAnswer(finalResponse.output_text || JSON.stringify(finalResponse, null, 2));
      } else {
        const reader = response.body?.getReader();
        if (!reader) throw Error('浏览器不支持流式响应');
        const decoder = new TextDecoder();
        let buffer = '';
        let finished = false;
        while (!finished) {
          const chunk = await reader.read();
          finished = chunk.done;
          buffer += decoder.decode(chunk.value || new Uint8Array(), { stream: !finished });
          const frames = buffer.split('\n\n');
          buffer = frames.pop() || '';
          for (const frame of frames) {
            const payload = frame.split('\n')
              .filter(line => line.startsWith('data:'))
              .map(line => line.slice(5).trim())
              .join('\n');
            if (!payload || payload === '[DONE]') continue;
            let event;
            try { event = JSON.parse(payload); } catch { continue; }
            if (event.type === 'response.output_text.delta' && event.delta) {
              if (!firstTokenAt) firstTokenAt = performance.now();
              setAnswer(current => current + event.delta);
            }
            if (event.type === 'response.completed' || event.type === 'response.incomplete') {
              finalResponse = event.response;
              if (!firstTokenAt && finalResponse?.output_text) firstTokenAt = performance.now();
              if (finalResponse?.output_text) {
                setAnswer(finalResponse.output_text);
              } else if (finalResponse?.output) {
                setAnswer(JSON.stringify(finalResponse.output, null, 2));
              }
            }
            if (event.type === 'response.failed') {
              throw Error(event.response?.error?.message || '上游响应失败');
            }
          }
        }
      }
      if (serial !== requestSerial.current) return;
      setRequestInfo({
        state: finalResponse?.status || 'completed',
        duration: performance.now() - startedAt,
        ttft: firstTokenAt ? firstTokenAt - startedAt : 0,
        usage: finalResponse?.usage || null,
        responseID: finalResponse?.id || '',
      });
      await refresh();
    } catch (error) {
      if (serial !== requestSerial.current) return;
      if (error.name === 'AbortError') {
        setRequestInfo({ state: 'cancelled', duration: performance.now() - startedAt, ttft: firstTokenAt ? firstTokenAt - startedAt : 0, usage: null });
        return;
      }
      setAnswer(error.message);
      setRequestInfo({ state: 'failed', duration: performance.now() - startedAt, ttft: firstTokenAt ? firstTokenAt - startedAt : 0, usage: null, error: error.message });
    } finally {
      if (serial === requestSerial.current) setLoading(false);
    }
  };

  // （账号操作 runAccountAction/runUpstreamAction/accountActionHandler 与账号表
  // columns 已随账号列表一起拆到 #/pool 独立页面 AccountList。）


  const requestColumns = [
    { title: '时间', dataIndex: 'created_at', render: value => value ? new Date(value * 1000).toLocaleString() : '-' },
    { title: '端点', dataIndex: 'route', render: value => <Text code>{value || '-'}</Text> },
    { title: '模型', dataIndex: 'model', render: value => <Text strong>{value || '-'}</Text> },
    { title: '模式', render: (_, record) => <Tag color={record.passthrough ? 'purple' : 'blue'}>{record.passthrough ? '透传' : record.mode === 'stream' ? '流式' : record.mode === 'sync' ? '同步' : record.mode || '-'}</Tag> },
    { title: '状态', dataIndex: 'status', render: value => <Tag color={value >= 200 && value < 300 ? 'green' : 'red'}>{value || '-'}</Tag> },
    { title: '输入', dataIndex: 'input_tokens', render: fmt },
    { title: '输出', dataIndex: 'output_tokens', render: fmt },
    { title: '总量', dataIndex: 'total_tokens', render: fmt },
    { title: '请求上限', dataIndex: 'requested_output_tokens', render: value => value ? fmt(value) : '-' },
    {
      title: '积分消耗',
      render: (_, record) => (
        <Space direction="vertical" size={0}>
          <Text>{record.credit_source && record.credit_source !== 'unknown' ? fmtCredits(record.credits_consumed) : '-'}</Text>
          <Text type="secondary">{{ upstream: '上游', estimated: '估算', unknown: '未知' }[record.credit_source] || '未知'}</Text>
        </Space>
      ),
    },
    { title: '首 token', dataIndex: 'ttfb_millis', render: value => value ? `${value}ms` : '-' },
    { title: '耗时', dataIndex: 'latency_millis', render: value => value ? `${(value / 1000).toFixed(2)}s` : '-' },
    { title: '账号', dataIndex: 'account_uid', render: value => value ? <Text code>{value.slice(0, 12)}</Text> : '-' },
    { title: '版本', dataIndex: 'account_region', render: value => <Tag color={value === 'global' ? 'gold' : value === 'cn' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : value === 'cn' ? '国内版' : '未知'}</Tag> },
    {
      title: '错误',
      render: (_, record) => record.error_code || record.error_message
        ? <Text type="danger" title={record.error_message || record.error_code}>{record.error_code || 'error'}</Text>
        : '-',
    },
  ];

  // filteredAccounts 支持按昵称/UID 搜索 + 状态/区域筛选，与表格排序组合使用。
  // （账号列表已拆到 #/pool 独立页面 AccountList，这里的筛选逻辑随迁。）


  // 指标口径：仪表盘的请求/token/积分统计按所选时间窗取值（来自
  // /requests?include=summary 的全量聚合，不受 limit 截断）。老后端没有
  // summary 字段时退回 /status 的累计指标（自进程/库启动以来的总数），
  // 此时标注"累计"以免口径混淆。缓存命中率与平均首 token 仍来自
  // /status metrics——它们是比率/均值，窗口化需要逐行聚合，留作后续扩展。
  const metrics = data.metrics || {};
  const cacheHitRate = Number(metrics.input_tokens || 0)
    ? (Number(metrics.cache_read_tokens || 0) / Number(metrics.input_tokens || 1)) * 100
    : 0;
  const windowed = dashboardSummary && Number(dashboardSummary.requests) > 0;
  const windowLabel = windowed ? { '1h': '（近 1 小时）', '6h': '（近 6 小时）', '24h': '（近 24 小时）', '7d': '（近 7 天）', '30d': '（近 30 天）' }[dashboardWindow] : '（累计）';
  const statSource = windowed ? {
    requests: dashboardSummary.requests,
    successes: dashboardSummary.successes,
    failures: dashboardSummary.failures,
    input_tokens: dashboardSummary.input_tokens,
    output_tokens: dashboardSummary.output_tokens,
    total_tokens: dashboardSummary.total_tokens,
    cache_read_tokens: dashboardSummary.cache_read_tokens,
    cache_write_tokens: dashboardSummary.cache_write_tokens,
    tool_calls: dashboardSummary.tool_calls,
    credits_consumed: dashboardSummary.credits_consumed,
  } : metrics;
  const statCards = [
    ['请求总数' + windowLabel, statSource.requests, '#7aa2ff'], ['成功请求', statSource.successes, '#52c41a'],
    ['失败请求', statSource.failures, '#ff7875'], ['输入 token', statSource.input_tokens, '#69c0ff'],
    ['输出 token', statSource.output_tokens, '#b37feb'], ['总 token', statSource.total_tokens, '#9254de'],
    ['缓存读取', statSource.cache_read_tokens, '#36cfc9'], ['缓存创建', statSource.cache_write_tokens, '#13c2c2'],
    ['工具调用', statSource.tool_calls, '#ffc53d'], ['积分消耗', statSource.credits_consumed, '#fa8c16', fmtCredits],
    // 剩余总余额：全池账号可用积分之和（非时间窗指标，实时快照）。
    ['剩余总余额', (data.accounts || []).reduce((sum, account) => sum + Number(account.credits || 0), 0), '#1677ff', fmtCredits],
  ];
  const activeTab = activeSection === 'settings' ? 'admin' : activeSection;
  const pageCopy = {
    dashboard: ['运营概览', '账号池、请求量和 token 用量实时汇总，数据每 30 秒自动更新。'],
    pool: ['账号列表', '账号池全量明细：状态、冷却/熔断、积分与凭证有效期，支持搜索和多维筛选。'],
    models: ['模型与请求', '查看当前可用的上游模型。'],
    playground: ['请求测试', '发送 Responses API 请求并查看标准化的 output_text。'],
    requests: ['请求日志', '按时间、模型、状态、错误码、端点、账号、版本、首字耗时和请求 ID 筛选历史请求。'],
    accounts: ['账号配置', '添加与授权账号：OAuth 登录链接、短信直登，以及账号池区域信息。'],
    'auto-enroll': ['自动加号', '用豪猪接码平台批量添加中国区账号：取号 → 发码 → 收码 → 自动落盘，全程免浏览器。'],
    proxy: ['注册代理', '短信直登链路的出口代理（旧模块）：池内容量、冷却状态。'],
    reqproxy: ['请求代理', '实际请求账号的代理池：v2rayN 订阅与多协议节点导入、按规则筛选、账号与槽位绑定，全部在此管理。'],
    groups: ['分组与密钥', '账号分组与 API 密钥管理：分组密钥只能使用绑定分组的账号；管理员密钥不受限。'],
    growth: ['成长任务', '猫猫旅行、连登兑换、抽奖与 17 项成长任务自动化：一键完成、批量执行、进度与领奖闭环。'],
    settings: ['管理设置', '配置签到与保活计划，手动触发签到和积分刷新。'],
  };
  const [pageTitle, pageDescription] = pageCopy[activeSection] || pageCopy.dashboard;
  const menuItems = [
    { key: 'dashboard', icon: <DashboardOutlined />, label: '仪表盘' },
    { key: 'pool', icon: <TeamOutlined />, label: '账号列表' },
    { key: 'models', icon: <ApiOutlined />, label: '模型目录' },
    { key: 'playground', icon: <SendOutlined />, label: '请求测试' },
    { key: 'requests', icon: <FileSearchOutlined />, label: '请求日志' },
    {
      key: 'accounts-group', icon: <UserAddOutlined />, label: '账号运营',
      type: 'group',
      children: [
        { key: 'accounts', icon: <KeyOutlined />, label: '账号配置' },
        { key: 'auto-enroll', icon: <ThunderboltOutlined />, label: '自动加号' },
        { key: 'proxy', icon: <CloudServerOutlined />, label: '注册代理' },
        { key: 'reqproxy', icon: <GlobalOutlined />, label: '请求代理' },
        { key: 'growth', icon: <GiftOutlined />, label: '成长任务' },
      ],
    },
    { key: 'groups', icon: <ApartmentOutlined />, label: '分组与密钥' },
    { key: 'settings', icon: <SettingOutlined />, label: '管理设置' },
  ];
  const tabItems = [
    {
      key: 'models', label: '模型目录',
      children: (
        <Card title="可用模型">
          {models.length
            ? <Space wrap>{models.map(model => <Tag key={model.id} color="blue">{model.id}</Tag>)}</Space>
            : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无可用模型" />}
        </Card>
      ),
    },
    {
      key: 'playground', label: '请求测试',
      children: (
        <Card title="Responses API 测试">
          <Space direction="vertical" size={14} style={{ width: '100%' }}>
            <div className="request-toolbar">
              <Select style={{ minWidth: 280 }} value={selectedModel} onChange={setSelectedModel} options={models.map(model => ({ value: model.id, label: model.id }))} />
              <Space><Switch aria-label="流式响应" checked={stream} onChange={setStream} /><Text type="secondary">流式响应</Text></Space>
            </div>
            <Input.TextArea rows={5} maxLength={200000} showCount value={promptText} onChange={event => setPromptText(event.target.value)} placeholder="输入测试内容" />
            <Space>
              <Button type="primary" icon={<SendOutlined />} loading={loading} disabled={!selectedModel || !promptText.trim()} onClick={send}>发送请求</Button>
              {loading && <Button onClick={() => requestController.current?.abort()}>停止</Button>}
            </Space>
            {requestInfo && (
              <div className="request-meta" aria-live="polite">
                <span>状态 <strong>{requestInfo.state}</strong></span>
                <span>耗时 <strong>{(requestInfo.duration / 1000).toFixed(2)}s</strong></span>
                <span>首 token <strong>{requestInfo.ttft ? `${(requestInfo.ttft / 1000).toFixed(2)}s` : '-'}</strong></span>
                <span>输入 <strong>{fmt(requestInfo.usage?.input_tokens)}</strong></span>
                <span>输出 <strong>{fmt(requestInfo.usage?.output_tokens)}</strong></span>
                {requestInfo.responseID && <span title={requestInfo.responseID}>响应 ID <strong>{requestInfo.responseID}</strong></span>}
              </div>
            )}
            <Card size="small" title="output_text"><pre className="response-output" aria-live="polite">{answer || '等待响应…'}</pre></Card>
          </Space>
        </Card>
      ),
    },
    {
      key: 'admin', label: '管理设置',
      children: (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Card title="定时任务">
            <Form form={form} layout="inline" onFinish={saveConfig}>
              <Form.Item name="checkin" label="签到小时"><Input placeholder="9,21" style={{ width: 90 }} /></Form.Item>
              <Form.Item name="travel" label="旅行小时"><Input placeholder="9,21" style={{ width: 90 }} /></Form.Item>
              <Form.Item name="activity" label="活跃上报小时"><Input placeholder="10" style={{ width: 70 }} /></Form.Item>
              <Form.Item name="keepalive" label="保活小时"><Input placeholder="22" style={{ width: 70 }} /></Form.Item>
              <Form.Item name="blackcat" label="夜猫子小时"><Input placeholder="23" style={{ width: 70 }} /></Form.Item>
              <Button type="primary" htmlType="submit">保存</Button>
            </Form>
            <Paragraph type="secondary" style={{ margin: '12px 0 0' }}>
              定时任务按上面的整点触发（逗号分隔多个时点）；留空的项保持现有配置不变。
              需要立刻执行时可点右侧按钮（对全部启用中的账号生效）。
              旅行 = 猫猫旅行巡检（领养/派出/领奖）；活跃上报 = 点亮连登 + 解锁领养前置；夜猫子 = 23:00–08:00 窗口对话补足。
            </Paragraph>
            <Space wrap style={{ marginTop: 8 }}>
              <Button icon={<CheckCircleOutlined />} loading={checkinRunning} onClick={runCheckinAll}>立即签到（含连登兑换/抽奖）</Button>
              <Button icon={<ReloadOutlined />} loading={creditRefreshing} onClick={refreshCredits}>立即刷新积分</Button>
            </Space>
            <div style={{ marginTop: 16 }}>
              <Space direction="vertical" size={10} style={{ width: '100%' }}>
                {[
                  ['checkin_enabled', '签到', '每日签到 + 余额刷新 + 连登兑换/抽奖闭环'],
                  ['travel_enabled', '猫猫旅行', '独立排程：无猫领养 / 空闲派出 / 到站领奖'],
                  ['activity_enabled', '活跃上报', '每日一条 chat_request_send 事件，点亮连登与任务解锁'],
                  ['keepalive_enabled', 'token 保活', '刷新全部账号 token，session 失效自动禁用'],
                  ['blackcat_enabled', '夜猫子', '23:00–08:00 窗口内 glm-5.2 对话补足（black_cat 任务）'],
                  ['autoenroll_growth_tasks', '加号后自动跑任务', '自动加号注册成功即自动执行 17 项成长任务（约 +1950 积分；含数条真实短对话）'],
                ].map(([key, label, desc]) => (
                  <div key={key} style={{ display: 'flex', justifyContent: 'space-between', gap: 24, alignItems: 'flex-start' }}>
                    <div>
                      <Text strong>{label}</Text>
                      <Paragraph type="secondary" style={{ margin: '4px 0 0' }}>{desc}</Paragraph>
                    </div>
                    <Switch
                      checked={config[key] !== false}
                      onChange={checked => saveScheduleToggle(key, checked)}
                    />
                  </div>
                ))}
              </Space>
            </div>
          </Card>
          {config.features && (
            <Card title="特性开关">
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <Paragraph type="secondary" style={{ margin: 0 }}>
                  保存后即时生效（同时写回 config.json，重启不丢）。只影响出站请求改写，不改变路由与账号选择。
                </Paragraph>
                {[
                  ['codex_compat', 'Codex 兼容', '改写 Codex CLI 系统提示词身份句（open source → open-source），绕开上游逐字指纹拦截；句子不存在时原样透传。'],
                  ['sanitize_blacklist_fingerprints', '指纹脱敏', '剥离/改写出站请求中的 Claude Code 指纹模板句；关闭后完全原样发送（调试用）。'],
                  ['passthrough', '原始流透传', '流式响应原样转发上游 SSE 字节，保留扩展字段；单请求可用 X-WorkBuddy-Passthrough: false 临时关闭。'],
                  ['responses_api', 'Responses API', '开放 /v1/responses 端点（OpenAI Responses 协议，Codex CLI 等客户端使用）；关闭后返回 404，只用 Chat Completions 的部署建议关掉。'],
                ].map(([key, label, desc]) => (
                  <div key={key} style={{ display: 'flex', justifyContent: 'space-between', gap: 24, alignItems: 'flex-start' }}>
                    <div>
                      <Text strong>{label}</Text>
                      <Paragraph type="secondary" style={{ margin: '4px 0 0' }}>{desc}</Paragraph>
                    </div>
                    <Switch
                      checked={!!config.features?.[key]}
                      onChange={checked => saveFeatures({ [key]: checked })}
                    />
                  </div>
                ))}
              </Space>
            </Card>
          )}
          {config.billing && (
            <Card title="积分估算费率">
              <Paragraph type="secondary" style={{ margin: '0 0 12px' }}>
                上游响应未带积分字段时按费率估算消耗（每 1,000 token 积分）。填 0 表示不估算。保存后即时生效。
              </Paragraph>
              <Form
                layout="inline"
                onFinish={values => saveBilling({
                  input_credits_per_1k_tokens: Number(values.input) || 0,
                  output_credits_per_1k_tokens: Number(values.output) || 0,
                  cached_input_credits_per_1k_tokens: Number(values.cached) || 0,
                })}
                initialValues={{
                  input: config.billing?.input_credits_per_1k_tokens ?? 0,
                  output: config.billing?.output_credits_per_1k_tokens ?? 0,
                  cached: config.billing?.cached_input_credits_per_1k_tokens ?? 0,
                }}
              >
                <Form.Item name="input" label="输入"><InputNumber min={0} step={0.001} style={{ width: 140 }} /></Form.Item>
                <Form.Item name="output" label="输出"><InputNumber min={0} step={0.001} style={{ width: 140 }} /></Form.Item>
                <Form.Item name="cached" label="缓存读取"><InputNumber min={0} step={0.001} style={{ width: 140 }} /></Form.Item>
                <Button type="primary" htmlType="submit">保存费率</Button>
              </Form>
            </Card>
          )}
          {config.upstream && (
            <Card title="上游请求超时（秒）">
              <Paragraph type="secondary" style={{ margin: '0 0 12px' }}>
                保存后即时生效，已在途请求不受影响（同时写回 config.json，重启不丢）。
                非流式超时也约束控制面调用（刷 token、签到、对账）。
              </Paragraph>
              <Form
                layout="inline"
                onFinish={values => saveUpstreamTimeouts({
                  timeout_seconds: Number(values.timeout) || 120,
                  stream_timeout_seconds: Number(values.streamTotal) || 0,
                  stream_idle_seconds: Number(values.streamIdle) || 120,
                })}
                initialValues={{
                  timeout: config.upstream?.timeout_seconds ?? 120,
                  streamTotal: config.upstream?.stream_timeout_seconds ?? 0,
                  streamIdle: config.upstream?.stream_idle_seconds ?? 120,
                }}
              >
                <Form.Item
                  name="timeout"
                  label={(
                    <Tooltip title="非流式请求（一次性拿完整响应）的整请求上限，含读完响应体。同时约束控制面调用。范围 10-3600。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>非流式超时</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber min={10} max={3600} style={{ width: 130 }} addonAfter="秒" />
                </Form.Item>
                <Form.Item
                  name="streamTotal"
                  label={(
                    <Tooltip title="流式请求总时长上限。0 = 不限（默认，长回答不会被掐断）。需要兜底（防跑飞的流长期占住账号）时设一个较大的值，范围 30-86400。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>流式总时长</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber min={0} max={86400} style={{ width: 150 }} addonAfter="秒" />
                </Form.Item>
                <Form.Item
                  name="streamIdle"
                  label={(
                    <Tooltip title="流式请求两次数据之间的最大间隔。上游持续产出时永不触发；卡住不发数据时据此尽快失败。0 = 关闭检查（不推荐）。范围 10-3600。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>流式空闲</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber min={0} max={3600} style={{ width: 130 }} addonAfter="秒" />
                </Form.Item>
                <Button type="primary" htmlType="submit">保存超时</Button>
              </Form>
            </Card>
          )}
          {config.max_request_body && (
            <Card title="请求体大小限制">
              <Paragraph type="secondary" style={{ margin: '0 0 12px' }}>
                /v1/chat/completions 与 /v1/responses 请求体的最大体积（含 Base64 图片）。
                保存后即时生效（同时写回 config.json，重启不丢）。调大前先确认服务器内存：
                请求体会完整读进内存再解析。带多张 Base64 图片的请求建议 16-32。
              </Paragraph>
              <Form
                layout="inline"
                onFinish={values => saveMaxRequestBody(Number(values.mib) || 8)}
                initialValues={{ mib: config.max_request_body?.mib ?? 8 }}
                key={config.max_request_body?.mib}
              >
                <Form.Item
                  name="mib"
                  label={(
                    <Tooltip title="请求体上限，单位 MiB。范围 1-64，默认 8。超限请求返回 413 request_too_large。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>上限</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber
                    min={config.max_request_body?.min_mib ?? 1}
                    max={config.max_request_body?.max_mib ?? 64}
                    style={{ width: 150 }}
                    addonAfter="MiB"
                  />
                </Form.Item>
                <Button type="primary" htmlType="submit">保存限制</Button>
              </Form>
            </Card>
          )}
          {config.request_log_retention && (
            <Card title="请求日志保留策略">
              <Paragraph type="secondary" style={{ margin: '0 0 12px' }}>
                两个条件**同时**生效，任一命中即删除（超过天数的旧行、超过条数的旧行）。
                保存后即时生效（下一次修剪按新值执行）。0 = 该条件不限。
                仪表盘时间窗统计基于这张表——保留量应大于日常统计窗口的请求量。
              </Paragraph>
              <Form
                layout="inline"
                onFinish={values => saveRequestLogRetention({
                  days: Number(values.days) || 0,
                  rows: Number(values.rows) || 0,
                })}
                initialValues={{
                  days: config.request_log_retention?.days ?? 0,
                  rows: config.request_log_retention?.rows ?? 10000,
                }}
              >
                <Form.Item
                  name="days"
                  label={(
                    <Tooltip title="保留最近 N 天的请求日志（0 = 不限时间，仅按条数）。范围 0-3650。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>保留天数</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber min={0} max={3650} style={{ width: 140 }} addonAfter="天" />
                </Form.Item>
                <Form.Item
                  name="rows"
                  label={(
                    <Tooltip title="最多保留的日志条数（0 = 不限条数，仅按时间）。范围 0-1000000。旧默认 1 万条。">
                      <span style={{ borderBottom: '1px dashed #bfbfbf' }}>最大条数</span>
                    </Tooltip>
                  )}
                >
                  <InputNumber min={0} max={1000000} style={{ width: 170 }} addonAfter="条" />
                </Form.Item>
                <Button type="primary" htmlType="submit">保存策略</Button>
              </Form>
            </Card>
          )}
        </Space>
      ),
    },
  ];

  return (
    <Layout style={{ minHeight: '100vh' }}>
        <Sider breakpoint="lg" collapsedWidth="0">
          <div style={{ color: '#172033', fontSize: 18, fontWeight: 700, padding: '22px 20px' }}>WorkBuddy<span style={{ color: '#356ae6' }}>2API</span></div>
          <Menu theme="light" mode="inline" selectedKeys={[activeSection]} items={menuItems} onClick={({ key }) => setActiveSection(key)} />
        </Sider>
        <Layout>
          <Header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 26px' }}>
            <Space>
              <Text strong style={{ color: '#172033' }}>服务控制台</Text>
              {/* healthy 是"可用账号数"，不是布尔量：池非空但全部冷却时同样不可服务，
                  因此这里用「可用数 + 在途占满」共同判定，不能直接拿它当 true/false。 */}
              {(() => {
                const total = Number(data.total || 0);
                const healthy = Number(data.healthy || 0);
                const full = Number(data.in_flight_full || 0);
                const servable = healthy > full;
                const label = !total ? '账号池为空'
                  : servable ? `服务可用（${healthy} 个账号）`
                    : healthy > 0 ? '账号在途占满' : '账号池检查中';
                return <Tag color={servable ? 'green' : 'orange'}>{label}</Tag>;
              })()}
            </Space>
            <Space>
              <Button icon={<ReloadOutlined />} onClick={refresh}>刷新</Button>
              <Button icon={<KeyOutlined />} onClick={() => {
                // setApiKey 已经足够；不再写 sessionStorage（HIGH-006）。
                const key = window.prompt('API Key（仅保留在当前页面内存中，刷新后需重新输入）', apiKey);
                if (key !== null) {
                  setApiKey(key);
                }
              }}>API Key</Button>
            </Space>
          </Header>
          <Content style={{ padding: 26 }}>
            <Title level={2} style={{ marginTop: 0 }}>{pageTitle}</Title>
            <Paragraph type="secondary">{pageDescription}</Paragraph>
            {activeSection === 'dashboard' && (
              <>
                {/* 时间窗选择：指标卡与最近请求都按这个窗口统计，默认 24 小时。 */}
                <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 12 }}>
                  <Space>
                    <Text type="secondary">统计时间窗</Text>
                    <Select
                      value={dashboardWindow}
                      onChange={setDashboardWindow}
                      style={{ width: 130 }}
                      options={[
                        { value: '1h', label: '近 1 小时' },
                        { value: '6h', label: '近 6 小时' },
                        { value: '24h', label: '近 24 小时' },
                        { value: '7d', label: '近 7 天' },
                        { value: '30d', label: '近 30 天' },
                      ]}
                    />
                  </Space>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(170px,1fr))', gap: 16, marginBottom: 20 }}>
                  {statCards.map(([label, value, color, formatter]) => <Card key={label}><Statistic title={label} value={formatter ? formatter(value) : fmt(value)} valueStyle={{ color }} /></Card>)}
                </div>
                <Card className="trend-card" title="性能趋势" extra={<Text type="secondary">最近 {metricSamples.length} 次刷新</Text>}>
                  <div className="trend-grid">
                    <div className="trend-layout">
                      <div>
                        <Text type="secondary">缓存命中率</Text>
                        <div className="trend-value">{cacheHitRate.toFixed(1)}%</div>
                        <Text type="secondary">缓存读取 token / 输入 token</Text>
                      </div>
                      <Sparkline values={metricSamples.map(sample => sample.cacheRate)} ariaLabel="缓存命中率趋势" />
                    </div>
                    <div className="trend-layout">
                      <div>
                        <Text type="secondary">平均首 token</Text>
                        <div className="trend-value">{Number(metrics.avg_ttfb_ms || 0) ? `${(Number(metrics.avg_ttfb_ms) / 1000).toFixed(2)}s` : '-'}</div>
                        <Text type="secondary">仅统计流式请求</Text>
                      </div>
                      <Sparkline values={metricSamples.map(sample => sample.avgTTFB)} color="#12a594" ariaLabel="平均首 token 趋势" />
                    </div>
                  </div>
                </Card>
                <Card
                  title="最近请求"
                  extra={(
                    <Space>
                      <Text type="secondary">{requestLogs.length} 条</Text>
                      <Button size="small" type="link" onClick={() => setActiveSection('requests')}>查看全部与筛选</Button>
                    </Space>
                  )}
                  style={{ marginTop: 20 }}
                >
                  <Table rowKey="id" columns={requestColumns} dataSource={requestLogs} pagination={{ pageSize: 10 }} scroll={{ x: 1480 }} locale={{ emptyText: '暂无请求记录' }} />
                </Card>
              </>
            )}
            {activeSection === 'requests' && <RequestLogsPage api={api} models={models} accounts={data.accounts || []} />}
            {activeSection === 'pool' && <AccountListPage api={api} data={data} refresh={refresh} refreshCredits={refreshCredits} creditRefreshing={creditRefreshing} />}
            {activeSection === 'accounts' && <AccountSettingsPage api={api} refresh={refresh} region={config.region} />}
            {activeSection === 'auto-enroll' && <AutoEnrollPage api={api} haozhumaSid={config.sms?.haozhuma?.sid || ''} onSaveHaozhumaSid={saveHaozhumaSid} />}
            {activeSection === 'groups' && <GroupsAndKeysPage api={api} />}
            {activeSection === 'proxy' && <ProxyPoolPage api={api} />}
            {activeSection === 'reqproxy' && <ReqProxyPage api={api} />}
            {activeSection === 'growth' && <GrowthTasksPage api={api} data={data} refresh={refresh} />}
            {tabItems.find(item => item.key === activeTab)?.children}
          </Content>
        </Layout>
        <Modal open={locked} title="控制台验证" onOk={unlock} onCancel={() => {}} okText="解锁" cancelButtonProps={{ style: { display: 'none' } }}>
          <Alert message="请输入管理员密钥（config 里的 api_key）。分组密钥不能解锁控制台。" type="info" showIcon style={{ marginBottom: 14 }} />
          <Input.Password prefix={<KeyOutlined />} value={apiKey} onChange={event => setApiKey(event.target.value)} onPressEnter={unlock} placeholder="输入管理员密钥" />
        </Modal>
    </Layout>
  );
}

// Root：ConfigProvider 内套 antd 的 App，让 Console 里的 useApp() 拿到
// 真正可用的 message / modal 实例（React 19 下静态方法不可用），同时继承主题。
function Root() {
  return (
    <ConfigProvider theme={{ token: { colorPrimary: '#356ae6', borderRadius: 8, colorBgContainer: '#ffffff', colorBgLayout: '#f5f7fb', colorText: '#172033', colorTextSecondary: '#667085', colorTextHeading: '#172033', colorBorder: '#e7ebf1' } }}>
      <AntApp>
        <Console />
      </AntApp>
    </ConfigProvider>
  );
}

createRoot(document.getElementById('root')).render(<Root />);
