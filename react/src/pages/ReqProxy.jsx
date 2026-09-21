import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert, Button, Card, Empty, Form, Input, InputNumber, Modal, Popconfirm, Select, Space,
  Switch, Table, Tabs, Tag, Timeline, Tooltip, Typography, message,
} from 'antd';
import { ReloadOutlined, PlusOutlined, ThunderboltOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// fmtBytes 订阅流量字节数转可读。
function fmtBytes(n) {
  const v = Number(n || 0);
  if (!v) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(v) / Math.log(1024)));
  return `${(v / 1024 ** i).toFixed(i ? 1 : 0)} ${units[i]}`;
}

const PROTO_COLORS = {
  vmess: 'geekblue', vless: 'purple', trojan: 'volcano', ss: 'cyan',
  socks: 'green', http: 'default', wireguard: 'orange',
};

function nodeStatus(n) {
  if (n.unhealthy) return <Tag color="red">不健康</Tag>;
  if (!n.probed) {
    // 区分"从没测过"和"测了但失败"：失败但未达摘除门槛（3 次）的
    // 显示失败次数，避免误读成"还没轮到测"。
    if (n.fail_streak > 0) return <Tooltip title={`探测失败 ${n.fail_streak} 次（连续 3 次将标记不健康并摘除 10 分钟）`}><Tag color="red">测速失败×{n.fail_streak}</Tag></Tooltip>;
    return <Tag>未测速</Tag>;
  }
  if (n.latency_ms < 0) return <Tag color="orange">超时</Tag>;
  return <Tag color="green">{n.latency_ms} ms</Tag>;
}

/**
 * ReqProxy 请求代理页（#/reqproxy）。
 *
 * 实际请求账号的代理池——订阅 / 节点 / 筛选规则 / 槽位与账号绑定四个维度，
 * 全部经 /admin/reqproxy/* 管理。与「代理池」（注册用 smslogin）完全独立。
 *
 * 数据流：
 *   subscriptions/nodes/slots/config 各自独立拉取；测速与刷新为异步动作
 *   （后端 202 语义：返回 ok 后前端轮询刷新列表）。
 */
export default function ReqProxy({ api }) {
  const [cfg, setCfg] = useState(null);
  const [subs, setSubs] = useState([]);
  const [nodes, setNodes] = useState([]);
  const [slots, setSlots] = useState([]);
  const [events, setEvents] = useState([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [subModal, setSubModal] = useState(null); // {mode:'add'|'edit', sub}
  const [importOpen, setImportOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [previewOpen, setPreviewOpen] = useState(false);
  const [previewURL, setPreviewURL] = useState('');
  const [previewData, setPreviewData] = useState(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [runningJobs, setRunningJobs] = useState({}); // kind → job（刷新后从后端恢复）
  const [form] = Form.useForm();

  const load = async () => {
    setLoading(true);
    try {
      const [config, subList, nodeList, slotList] = await Promise.all([
        api('/admin/reqproxy/config'),
        api('/admin/reqproxy/subscriptions').catch(() => ({ data: [] })),
        api('/admin/reqproxy/nodes').catch(() => ({ data: [] })),
        api('/admin/reqproxy/slots').catch(() => ({ data: [] })),
      ]);
      setCfg(config);
      setSubs(subList.data || []);
      setNodes(nodeList.data || []);
      setSlots(slotList.data || []);
      setError('');
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadEvents = async () => {
    try {
      const body = await api('/admin/reqproxy/events');
      setEvents((body.data || []).slice().reverse());
    } catch { /* 事件拉取失败不打断主界面 */ }
  };

  // 任务轮询：驱动长动作按钮的 loading。状态在后端，页面刷新后
  // 首轮立即恢复 running 动画，任务完成时弹结果并刷新数据。
  //
  // 乐观登记（optimistic ref）：点击动作时立即记下 job_id 起动画。
  // 轮询响应里：该 id 已 running → 服务端确认，撤乐观标记；
  // 该 id done/error → 弹结果、撤标记；一直查不到（登记竞态/被裁剪）
  // → 30 秒宽限后放弃。UI 的 running = 服务端 running ∪ 乐观标记。
  // 这样晚到的旧响应（不含新任务）不会把动画提前关掉。
  const optimisticRef = useRef({}); // job_id → { kind, at }
  const notifiedRef = useRef({});   // job_id → true（已弹过结果）
  const lastRunningRef = useRef({}); // 上一帧服务端 running（完成检测用）
  // 启动窗口保护：kind → true。按钮点击到 POST 返回、job_id 登记
  // optimisticRef 之间（百毫秒级）并发的 pollJobs 会用服务端快照覆盖
  // runningJobs，把刚点的动画清掉——启动窗口内的 kind 不参与覆盖。
  const startingRef = useRef({});

  const pollJobs = async () => {
    let jobs = [];
    try {
      const body = await api('/admin/reqproxy/jobs?limit=50');
      jobs = body.data || [];
    } catch { /* 拉取失败：保持现状继续轮询 */ }

    const byID = {};
    jobs.forEach(j => { byID[j.id] = j; });

    // 乐观标记结算
    const now = Date.now();
    Object.entries(optimisticRef.current).forEach(([id, info]) => {
      const j = byID[id];
      if (j && j.state === 'running') {
        delete optimisticRef.current[id]; // 服务端已确认，后续由服务端状态接管
      } else if (j && j.state !== 'running') {
        // 完成得比轮询还快：直接弹结果
        if (!notifiedRef.current[id]) {
          notifiedRef.current[id] = true;
          if (j.state === 'error') message.error(`${j.label}失败：${j.note}`);
          else message.success(`${j.label}完成：${j.note}`);
          load();
        }
        delete optimisticRef.current[id];
      } else if (now - info.at > 30000) {
        delete optimisticRef.current[id]; // 宽限超时（登记竞态/列表裁剪）
      }
    });

    // 完成→弹结果（针对服务端确认过的 running 任务）
    const running = {};
    jobs.forEach(j => {
      if (j.state === 'running') running[j.kind] = j;
      else if (lastRunningRef.current[j.id] && !notifiedRef.current[j.id]) {
        notifiedRef.current[j.id] = true;
        if (j.state === 'error') message.error(`${j.label}失败：${j.note}`);
        else message.success(`${j.label}完成：${j.note}`);
        load();
      }
    });
    lastRunningRef.current = running;

    // UI 状态 = 服务端 running ∪ 乐观标记 ∪ 启动窗口保护
    const ui = { ...running };
    Object.values(optimisticRef.current).forEach(info => {
      if (!ui[info.kind]) ui[info.kind] = { kind: info.kind, state: 'running' };
    });
    Object.keys(startingRef.current).forEach(kind => {
      if (!ui[kind]) ui[kind] = { kind, state: 'running' };
    });
    setRunningJobs(ui);

    return Object.keys(ui).length > 0;
  };

  useEffect(() => {
    let stopped = false;
    let timer = null;
    const tick = async () => {
      const hasRunning = await pollJobs();
      if (stopped) return;
      // running/乐观标记期间 1s 高频轮询；空闲 5s 低频兜底（刷新页面后
      // 能在 5 秒内恢复动画）
      timer = setTimeout(tick, hasRunning ? 1000 : 5000);
    };
    tick();
    return () => { stopped = true; clearTimeout(timer); };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); loadEvents(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // ---- 订阅操作 ----
  const saveSub = async values => {
    try {
      if (subModal.mode === 'add') {
        await api('/admin/reqproxy/subscriptions', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(values),
        });
        message.success('订阅已添加，正在首次拉取…');
        // 后台立即刷新一次拉节点
        const list = await api('/admin/reqproxy/subscriptions');
        const added = (list.data || []).slice(-1)[0];
        if (added) {
          api(`/admin/reqproxy/subscriptions/${added.id}/refresh`, { method: 'POST' })
            .then(() => { message.success('订阅拉取完成'); load(); })
            .catch(err => { message.error(`拉取失败: ${err.message}`); load(); });
        }
      } else {
        await api(`/admin/reqproxy/subscriptions/${subModal.sub.id}`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(values),
        });
        message.success('订阅已更新');
      }
      setSubModal(null);
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const refreshSub = async id => {
    startingRef.current['sub-refresh'] = true;
    setRunningJobs(p => ({ ...p, 'sub-refresh': { kind: 'sub-refresh', state: 'running' } }));
    try {
      const body = await api(`/admin/reqproxy/subscriptions/${id}/refresh`, { method: 'POST' });
      delete startingRef.current['sub-refresh'];
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'sub-refresh', at: Date.now() };
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current['sub-refresh'];
      setRunningJobs(p => { const q = { ...p }; delete q['sub-refresh']; return q; });
      message.error(`刷新失败: ${err.message}`);
      load();
    }
  };

  const deleteSub = async id => {
    try {
      await api(`/admin/reqproxy/subscriptions/${id}`, { method: 'DELETE' });
      message.success('已删除（其节点已移出池）');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  // 整合槽位：同节点多槽合并、删空槽（任务化，动画由 jobs 轮询驱动）
  const compactSlots = async () => {
    startingRef.current.compact = true;
    setRunningJobs(p => ({ ...p, compact: { kind: 'compact', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/compact', { method: 'POST' });
      delete startingRef.current.compact;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'compact', at: Date.now() };
      } else {
        message.info('整合已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.compact;
      setRunningJobs(p => { const q = { ...p }; delete q.compact; return q; });
      message.error(err.message);
    }
  };

  // 重新分配槽位：清空全部槽位/绑定，按当前规则从头分配（任务化）
  const reassignSlots = async () => {
    startingRef.current.reassign = true;
    setRunningJobs(p => ({ ...p, reassign: { kind: 'reassign', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/reassign', { method: 'POST' });
      delete startingRef.current.reassign;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'reassign', at: Date.now() };
      } else {
        message.info('重分配已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.reassign;
      setRunningJobs(p => { const q = { ...p }; delete q.reassign; return q; });
      message.error(err.message);
    }
  };

  const doPreview = async () => {
    if (!previewURL.trim()) return;
    setPreviewLoading(true);
    try {
      const body = await api('/admin/reqproxy/subscriptions/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: previewURL.trim() }),
      });
      setPreviewData(body);
    } catch (err) {
      message.error(err.message);
    } finally {
      setPreviewLoading(false);
    }
  };

  // ---- 节点操作 ----
  const doImport = async () => {
    try {
      const body = await api('/admin/reqproxy/nodes/import', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: importText }),
      });
      message.success(`已导入 ${body.added} 个节点`);
      setImportOpen(false);
      setImportText('');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const deleteNode = async id => {
    try {
      await api(`/admin/reqproxy/nodes/${id}`, { method: 'DELETE' });
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const probeAll = async () => {
    // 乐观动画在请求发出前就位：POST 网络往返期间并发在途的 pollJobs
    // 会用服务端快照覆盖 runningJobs（此刻任务还没登记进列表），
    // startingRef 保护这个窗口。
    startingRef.current['health-run'] = true;
    setRunningJobs(p => ({ ...p, 'health-run': { kind: 'health-run', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/health/run', { method: 'POST' });
      delete startingRef.current['health-run'];
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'health-run', at: Date.now() };
        message.success('全量测速已开始（结束后自动预热+再平衡）');
      } else {
        // 同类任务已在跑：动画保持，服务端 running 状态下一帧轮询接管
        message.info('全量测速已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current['health-run'];
      setRunningJobs(p => { const q = { ...p }; delete q['health-run']; return q; });
      message.error(err.message);
    }
  };

  const rebalance = async () => {
    startingRef.current.rebalance = true;
    setRunningJobs(p => ({ ...p, rebalance: { kind: 'rebalance', state: 'running' } }));
    try {
      const body = await api('/admin/reqproxy/rebalance', { method: 'POST' });
      delete startingRef.current.rebalance;
      if (body.job_id) {
        optimisticRef.current[body.job_id] = { kind: 'rebalance', at: Date.now() };
      } else {
        message.info('再平衡已在进行中');
      }
      pollJobs();
    } catch (err) {
      delete startingRef.current.rebalance;
      setRunningJobs(p => { const q = { ...p }; delete q.rebalance; return q; });
      message.error(err.message);
    }
  };

  // 单节点测速：后端同步探测（秒级），按钮动画持续到结果返回。
  const [probingID, setProbingID] = useState(null);
  const probeOne = async id => {
    setProbingID(id);
    try {
      const body = await api(`/admin/reqproxy/health/run/${id}`, { method: 'POST' });
      if (body && body.ok) {
        message.success(`测速完成：${body.latency_ms} ms`);
      } else {
        message.error(`测速失败：${body?.error || '未知错误'}`);
      }
      load();
    } catch (err) {
      message.error(err.message);
    } finally {
      setProbingID(null);
    }
  };

  // ---- 总开关 ----
  const toggleEnabled = async checked => {
    try {
      await api('/admin/reqproxy/config', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: checked }),
      });
      message.success(checked ? '请求代理已启用：绑定账号的流量将走槽位出口' : '已停用：全部直连');
      load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const subColumns = [
    { title: '名称', dataIndex: 'name', width: 120 },
    {
      title: '订阅地址', dataIndex: 'url', ellipsis: true,
      render: v => <Text code style={{ fontSize: 12 }}>{v}</Text>,
    },
    { title: '刷新间隔', dataIndex: 'interval', width: 90 },
    { title: '自动刷新', dataIndex: 'auto_refresh', width: 90, render: v => (v ? <Tag color="blue">开</Tag> : <Tag>关</Tag>) },
    {
      title: '流量 / 到期', width: 190,
      render: (_, s) => {
        const ui = s.userinfo;
        if (!ui) return <Text type="secondary">-</Text>;
        const used = Number(ui.upload_bytes || 0) + Number(ui.download_bytes || 0);
        const total = Number(ui.total_bytes || 0);
        const pct = total ? Math.min(100, (used / total) * 100) : 0;
        const low = total && pct >= 90;
        return (
          <Space direction="vertical" size={2}>
            {total > 0 && <Text type={low ? 'danger' : 'secondary'}>{fmtBytes(used)} / {fmtBytes(total)}</Text>}
            {ui.expire_at && <Text type="secondary">到期 {new Date(ui.expire_at).toLocaleDateString()}</Text>}
          </Space>
        );
      },
    },
    {
      title: '状态', dataIndex: 'last_status', width: 100,
      render: (v, s) => (v === 'ok'
        ? <Tag color="green">正常</Tag>
        : <Tooltip title={s.last_error}><Tag color={s.last_status ? 'red' : 'default'}>{s.last_status ? '失败' : '未拉取'}</Tag></Tooltip>),
    },
    {
      title: '操作', width: 200,
      render: (_, s) => (
        <Space size={4}>
          <Button size="small" icon={<ReloadOutlined />} loading={!!runningJobs['sub-refresh']} disabled={busy && !runningJobs['sub-refresh']} onClick={() => refreshSub(s.id)}>刷新</Button>
          {guard(<Button size="small" disabled={busy} onClick={() => { setSubModal({ mode: 'edit', sub: s }); form.setFieldsValue(s); }}>编辑</Button>)}
          {guard(<Popconfirm title="删除该订阅？其节点将移出池，相关槽位自动换指向。" onConfirm={() => deleteSub(s.id)}>
            <Button size="small" danger disabled={busy}>删除</Button>
          </Popconfirm>)}
        </Space>
      ),
    },
  ];

  const nodeColumns = [
    { title: '节点名', dataIndex: 'name', ellipsis: true, render: v => <Text strong>{v}</Text> },
    { title: '协议', dataIndex: 'protocol', width: 90, render: v => <Tag color={PROTO_COLORS[v] || 'default'}>{v}</Tag> },
    { title: '地区', dataIndex: 'region', width: 70, render: v => (v === 'other' ? '-' : v) },
    { title: '来源', dataIndex: 'source', width: 110, ellipsis: true, render: v => (v === 'manual' ? '手动导入' : v) },
    { title: '被指向槽位', dataIndex: 'pointed_slots', width: 100, render: v => (v ? <Tag color={v > 1 ? 'orange' : 'blue'}>{v}</Tag> : '-') },
    { title: '累计请求', dataIndex: 'requests', width: 100, sorter: (a, b) => (a.requests || 0) - (b.requests || 0), render: v => (Number(v || 0) > 0 ? <Text>{Number(v).toLocaleString()}</Text> : <Text type="secondary">-</Text>) },
    { title: '延迟', width: 110, render: (_, n) => nodeStatus(n) },
    {
      title: '操作', width: 130,
      render: (_, n) => (
        <Space size={4}>
          <Button size="small" loading={probingID === n.id} disabled={busy && probingID !== n.id} onClick={() => probeOne(n.id)}>测速</Button>
          {n.source === 'manual' && (
            guard(<Popconfirm title="删除该节点？" onConfirm={() => deleteNode(n.id)}>
              <Button size="small" danger disabled={busy}>删除</Button>
            </Popconfirm>)
          )}
        </Space>
      ),
    },
  ];

  const slotColumns = [
    { title: '槽位', dataIndex: 'id', width: 130, render: v => <Text code>{v}</Text> },
    { title: 'region', dataIndex: 'region', width: 80, render: v => <Tag color={v === 'cn' ? 'blue' : 'purple'}>{v}</Tag> },
    { title: '本地端口', dataIndex: 'port', width: 90, render: v => <Text code>{v}</Text> },
    {
      title: '当前出口节点', width: 200,
      render: (_, s) => (s.node
        ? <Space size={4}><Text strong>{s.node.name}</Text>{s.node.unhealthy ? <Tag color="red">不健康</Tag> : <Tag color="green">{s.node.latency_ms >= 0 ? `${s.node.latency_ms}ms` : ''}</Tag>}</Space>
        : <Tag>空槽</Tag>),
    },
    {
      title: '绑定账号', dataIndex: 'accounts', width: 140,
      render: accounts => (accounts?.length ? <Tag color="blue">{accounts.length} 个账号</Tag> : <Text type="secondary">无</Text>),
    },
    {
      title: '操作', width: 150,
      render: (_, s) => (
        <Select
          size="small" style={{ minWidth: 130 }} placeholder="手动换指向"
          disabled={busy}
          showSearch optionFilterProp="label"
          options={nodes.filter(n => !n.unhealthy).map(n => ({ value: n.id, label: `${n.name}（${n.latency_ms >= 0 ? `${n.latency_ms}ms` : '未测'}）` }))}
          onChange={async nodeID => {
            try {
              await api(`/admin/reqproxy/slots/${s.id}`, {
                method: 'PUT', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ node_id: nodeID }),
              });
              message.success('槽位已换指向');
              load();
            } catch (err) { message.error(err.message); }
          }}
        />
      ),
    },
  ];

  const enabled = cfg?.enabled === true;

  // 任务互斥：任何长任务（测速/再平衡/整合/重分配/预热/订阅刷新）运行期间，
  // 禁用所有会改动节点池或槽位拓扑的操作——并发跑会互相干扰（测速中途
  // 删节点→探测悬空；重分配中途导入→槽位错乱）。
  const busy = !!(runningJobs['health-run'] || runningJobs.rebalance ||
    runningJobs.compact || runningJobs.reassign || runningJobs.prewarm ||
    runningJobs['sub-refresh'] || probingID);
  const busyLabel = runningJobs['health-run'] ? '全量测速'
    : runningJobs.rebalance ? '负载再平衡'
    : runningJobs.compact ? '整合槽位'
    : runningJobs.reassign ? '重新分配'
    : runningJobs.prewarm ? '账号预热'
    : runningJobs['sub-refresh'] ? '订阅刷新'
    : probingID ? '单节点测速' : '';
  // busy 时禁用并提示哪个任务在跑（Tooltip 在禁用按钮上仍可悬浮）。
  const guard = el => (busy
    ? <Tooltip title={`${busyLabel}进行中，此操作暂不可用`}>{el}</Tooltip>
    : el);

  const tabItems = [
    {
      key: 'subs', label: '订阅管理', children: (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap>
            {guard(<Button type="primary" icon={<PlusOutlined />} disabled={busy} onClick={() => { form.resetFields(); setSubModal({ mode: 'add' }); }}>添加订阅</Button>)}
            <Button onClick={() => { setPreviewData(null); setPreviewURL(''); setPreviewOpen(true); }}>预览订阅</Button>
          </Space>
          {subs.length
            ? <Table rowKey="id" columns={subColumns} dataSource={subs} pagination={false} />
            : <Empty description="尚无订阅。添加一个 v2rayN 订阅地址，或到「节点池」手动导入。" />}
        </Space>
      ),
    },
    {
      key: 'nodes', label: `节点池（${nodes.length}）`, children: (
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap>
            {guard(<Button type="primary" disabled={busy} onClick={() => setImportOpen(true)}>批量导入</Button>)}
            <Button icon={<ThunderboltOutlined />} loading={!!runningJobs['health-run']} disabled={busy && !runningJobs['health-run']} onClick={probeAll}>全量测速</Button>
            <Button loading={!!runningJobs.rebalance} disabled={busy && !runningJobs.rebalance} onClick={rebalance}>负载再平衡</Button>
            <Tooltip title="合并同节点的重复槽位、删除空槽位。不动节点选择，只收敛拓扑。">
              <Button loading={!!runningJobs.compact} disabled={busy && !runningJobs.compact} onClick={compactSlots}>整合槽位</Button>
            </Tooltip>
            <Popconfirm title="清空全部槽位和绑定，按当前筛选规则从头重新分配账号？"
              description="已有 pin 的账号保持不动；期间请求走直连兜底。"
              okText="重分配" cancelText="取消" onConfirm={reassignSlots}>
              <Button danger loading={!!runningJobs.reassign} disabled={busy && !runningJobs.reassign}>重新分配槽位</Button>
            </Popconfirm>
            <Text type="secondary">支持 share-link（vmess/vless/trojan/ss/socks5/http）、Base64 列表、host:port:user:pass</Text>
          </Space>
          {nodes.length
            ? <Table rowKey="id" columns={nodeColumns} dataSource={nodes} pagination={{ pageSize: 15 }} />
            : <Empty description="节点池为空。未测速的节点不参与分配。" />}
        </Space>
      ),
    },
    {
      key: 'rules', label: '筛选规则', children: (
        <RulesTab api={api} cfg={cfg} onSaved={load} nodes={nodes} />
      ),
    },
    {
      key: 'slots', label: `槽位与绑定（${slots.length}）`, children: (
        slots.length
          ? <Table rowKey="id" columns={slotColumns} dataSource={slots} pagination={false} />
          : <Empty description="尚无槽位。启用模块后，账号第一次发请求时会自动分配。" />
      ),
    },
    {
      key: 'events', label: '事件流', children: (
        events.length
          ? <Timeline items={events.slice(0, 50).map(e => ({
              color: e.kind === 'warn' ? 'red' : e.kind === 'slot-switch' ? 'blue' : 'green',
              children: (
                <Space direction="vertical" size={0}>
                  <Text>{e.msg}</Text>
                  <Text type="secondary" style={{ fontSize: 12 }}>{new Date(e.at).toLocaleString()}</Text>
                </Space>
              ),
            }))} />
          : <Empty description="暂无事件" />
      ),
    },
  ];

  if (error) return <Alert type="error" message={error} showIcon />;

  return (
    <Space direction="vertical" size={14} style={{ width: '100%' }}>
      <Card size="small">
        <Space wrap size={16}>
          <Space>
            <Tooltip title={busy ? `${busyLabel}进行中，暂不能切换` : ''}>
              <Switch checked={enabled} disabled={busy} onChange={toggleEnabled} />
            </Tooltip>
            <Text strong>{enabled ? '请求代理：已启用' : '请求代理：已停用（全部直连）'}</Text>
          </Space>
          {cfg && (
            <>
              <Text type="secondary">订阅 {cfg.subscriptions} · 手动节点 {cfg.manual_nodes} · 槽位 {cfg.slots} · 绑定 {cfg.bindings}</Text>
              <Button size="small" icon={<ReloadOutlined />} onClick={() => { load(); loadEvents(); }} loading={loading}>刷新</Button>
            </>
          )}
        </Space>
      </Card>

      {!enabled && subs.length > 0 && (
        <Alert type="info" showIcon message="模块未启用：账号请求全部直连。启用后，绑定账号的流量将走其槽位的代理出口。" />
      )}

      <Tabs items={tabItems} defaultActiveKey="subs" />

      {/* 订阅编辑弹窗 */}
      <Modal
        open={!!subModal} title={subModal?.mode === 'add' ? '添加订阅' : '编辑订阅'}
        onCancel={() => setSubModal(null)} onOk={() => form.submit()} destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={saveSub}>
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '必填' }]}>
            <Input placeholder="机场A" />
          </Form.Item>
          <Form.Item name="url" label="订阅地址" rules={[{ required: true, message: '必填' }]}>
            <Input placeholder="https://example.com/api/v1/client/subscribe?token=..." />
          </Form.Item>
          <Space size={12}>
            <Form.Item name="auto_refresh" label="自动刷新" valuePropName="checked" initialValue={true}>
              <Switch />
            </Form.Item>
            <Form.Item name="interval" label="刷新间隔" initialValue="1h">
              <Select style={{ width: 120 }} options={['15m', '30m', '1h', '3h', '6h', '12h', '24h'].map(v => ({ value: v, label: v }))} />
            </Form.Item>
          </Space>
        </Form>
      </Modal>

      {/* 订阅预览弹窗 */}
      <Modal open={previewOpen} title="预览订阅（不入池）" onCancel={() => setPreviewOpen(false)} footer={null}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space.Compact style={{ width: '100%' }}>
            <Input placeholder="订阅地址" value={previewURL} onChange={e => setPreviewURL(e.target.value)} />
            <Button type="primary" loading={previewLoading} onClick={doPreview}>拉取预览</Button>
          </Space.Compact>
          {previewData && (
            <>
              <Alert type={previewData.total ? 'success' : 'warning'} showIcon
                message={`解析出 ${previewData.total} 个节点${(previewData.warnings || []).length ? `；${previewData.warnings.join('；')}` : ''}`} />
              {previewData.userinfo && (
                <Text type="secondary">
                  流量 {fmtBytes(Number(previewData.userinfo.upload_bytes || 0) + Number(previewData.userinfo.download_bytes || 0))}
                  / {fmtBytes(previewData.userinfo.total_bytes)}
                  {previewData.userinfo.expire_at ? ` · 到期 ${new Date(previewData.userinfo.expire_at).toLocaleDateString()}` : ''}
                </Text>
              )}
              <div style={{ maxHeight: 300, overflow: 'auto' }}>
                {(previewData.nodes || []).map((n, i) => (
                  <div key={i}><Text>{n.name}</Text> <Tag color={PROTO_COLORS[n.protocol]}>{n.protocol}</Tag> {n.region !== 'other' && <Tag>{n.region}</Tag>}</div>
                ))}
              </div>
            </>
          )}
        </Space>
      </Modal>

      {/* 手动导入弹窗 */}
      <Modal open={importOpen} title="批量导入节点" onCancel={() => setImportOpen(false)} onOk={doImport} okText="导入">
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Text type="secondary">每行一条：share-link / socks5:// / http:// 或 host:port:user:pass；也可粘贴整段 Base64。</Text>
          <Input.TextArea rows={8} value={importText} onChange={e => setImportText(e.target.value)}
            placeholder={'vmess://eyJ2IjoiMiIsInBzIjoi6aaW5a2mMDEiLCJhZGQiOiJ4eC5leGFtcGxlLmNvbSIsInBvcnQiOiI0NDMiLCJpZCI6InV1aWQiLCJuZXQiOiJ3cyJ9\ntrojan://pass@xx.example.com:443#东京01'} />
        </Space>
      </Modal>
    </Space>
  );
}

/**
 * RulesTab 筛选规则 Tab：草稿编辑 + 预览模拟 + 应用。
 */
function RulesTab({ api, cfg, onSaved }) {
  const [form] = Form.useForm();
  const [preview, setPreview] = useState(null);
  const [previewing, setPreviewing] = useState(false);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (cfg?.rules) {
      form.setFieldsValue({
        include_keywords: (cfg.rules.include_keywords || []).join(','),
        exclude_keywords: (cfg.rules.exclude_keywords || []).join(','),
        max_latency_ms: cfg.rules.max_latency_ms || 0,
        accounts_per_node: cfg.rules.accounts_per_node || 0,
        region_cn: (cfg.rules.region_rules?.cn || []).join(','),
        region_global: (cfg.rules.region_rules?.global || []).join(','),
      });
    }
  }, [cfg, form]);

  const draftRules = () => {
    const v = form.getFieldsValue();
    return {
      include_keywords: (v.include_keywords || '').split(',').map(s => s.trim()).filter(Boolean),
      exclude_keywords: (v.exclude_keywords || '').split(',').map(s => s.trim()).filter(Boolean),
      max_latency_ms: Number(v.max_latency_ms || 0),
      accounts_per_node: Number(v.accounts_per_node || 0),
      region_rules: {
        ...(v.region_cn ? { cn: v.region_cn.split(',').map(s => s.trim()).filter(Boolean) } : {}),
        ...(v.region_global ? { global: v.region_global.split(',').map(s => s.trim()).filter(Boolean) } : {}),
      },
    };
  };

  const doPreview = async () => {
    setPreviewing(true);
    try {
      const body = await api('/admin/reqproxy/rules/preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ rules: draftRules() }),
      });
      setPreview(body);
    } catch (err) {
      message.error(err.message);
    } finally {
      setPreviewing(false);
    }
  };

  const apply = async () => {
    setSaving(true);
    try {
      await api('/admin/reqproxy/config', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ rules: draftRules() }),
      });
      message.success('规则已应用');
      onSaved?.();
    } catch (err) {
      message.error(err.message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Space direction="vertical" size={14} style={{ width: '100%' }}>
      <Card title="规则（草稿，应用前不影响分配）">
        <Form form={form} layout="vertical">
          <Form.Item name="include_keywords" label="名称白名单（逗号分隔；空 = 不过滤）">
            <Input placeholder="香港,日本" />
          </Form.Item>
          <Form.Item name="exclude_keywords" label="名称黑名单（逗号分隔）">
            <Input placeholder="剩余流量,到期,官网,套餐" />
          </Form.Item>
          <Space size={16} wrap>
            <Form.Item name="max_latency_ms" label="延迟上限 ms（0 = 不限）">
              <InputNumber min={0} step={100} style={{ width: 120 }} />
            </Form.Item>
            <Form.Item name="accounts_per_node" label="共享度：账号/节点（0 = 不限）">
              <InputNumber min={0} step={1} style={{ width: 120 }} />
            </Form.Item>
          </Space>
          <Space size={16} wrap>
            <Form.Item name="region_cn" label="cn 账号允许的节点地区（逗号分隔；空 = 不限）">
              <Input placeholder="HK,JP,SG" style={{ width: 220 }} />
            </Form.Item>
            <Form.Item name="region_global" label="global 账号允许的节点地区">
              <Input placeholder="US" style={{ width: 220 }} />
            </Form.Item>
          </Space>
          <Space>
            <Button onClick={doPreview} loading={previewing}>预览模拟结果</Button>
            <Button type="primary" onClick={apply} loading={saving}>应用规则</Button>
          </Space>
        </Form>
      </Card>

      {preview && (
        <Card title="模拟结果">
          <Space direction="vertical" size={6}>
            <Text>全池 <Text strong>{preview.total}</Text> 个节点 → 规则生效后 <Text strong style={{ color: '#1677ff' }}>{preview.qualified}</Text> 个合格</Text>
            <Text type="secondary">
              过滤分布：{Object.entries(preview.filtered_out || {}).map(([k, v]) => `${k}×${v}`).join('，') || '无'}
            </Text>
            <Text type="secondary">
              按来源：{Object.entries(preview.by_source || {}).map(([k, v]) => `${k === 'manual' ? '手动' : k} ${v} 个`).join('；') || '无'}
            </Text>
            <Text type="secondary">
              地区合格数：{Object.entries(preview.regions || {}).map(([k, v]) => `${k} 区 ${v} 个`).join('；') || '无'}
            </Text>
            <Text type="secondary">
              共享度预估：{Object.entries(preview.shared_per_node || {}).map(([k, v]) => `${k} 区约 ${v} 账号/节点`).join('；') || '暂无绑定'}
            </Text>
            {(preview.notes || []).map((n, i) => <Alert key={i} type="warning" showIcon message={n} />)}
          </Space>
        </Card>
      )}
    </Space>
  );
}
