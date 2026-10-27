import React, { useEffect, useRef, useState } from 'react';
import {
  Alert, Button, Card, Empty, InputNumber, Popconfirm, Select, Space, Switch,
  Table, Tag, Tooltip, Typography, message,
} from 'antd';
import { EyeOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';

import ProjectPickerModal from './ProjectPickerModal';
import WatchHistoryPanel from './WatchHistoryPanel';

const { Text } = Typography;

// WATCH_POLL_MS 监控状态轮询间隔（10s：值班员的节奏是分钟级，10s 刷新
// 状态/日志足够流畅，也不至于打爆接口）。
const WATCH_POLL_MS = 10000;

/**
 * CommitInputNumber 失焦/回车才提交的数字输入。
 *
 * 直接 onChange 即存的写法在输入中途就会触发保存——想输 120 会先存 1
 * （后端报"间隔需 60-3600"）、再存 12（又报错）。改为：编辑期间只更新
 * 本地值（也不被 10s 轮询回来的服务器值覆盖），失焦或回车才调
 * onCommit；validate 不过（含清空）就回退到原值并提示，不打后端。
 */
function CommitInputNumber({ value, onCommit, validate, formatMessage, ...rest }) {
  const [editing, setEditing] = useState(null); // null = 未在编辑（跟随 props 值）
  const commit = (v) => {
    setEditing(null);
    if (validate && !validate(v)) {
      if (formatMessage) message.warning(formatMessage(v));
      return; // 放弃本次编辑，自然回退到 props value
    }
    if (v !== value) onCommit(v);
  };
  return (
    <InputNumber
      {...rest}
      value={editing === null ? value : editing}
      onChange={v => setEditing(v)}
      onPressEnter={() => { if (editing !== null) commit(editing); }}
      onBlur={() => { if (editing !== null) commit(editing); }}
    />
  );
}

/**
 * WatchCard 对接码监控卡片（UID Watcher）。
 *
 * 后台值班员：定期拉对接码市场 → 发现"新码/降价进区间/补货"事件 →
 * 用该码自动加号 → 从额度里扣真实花费；额度耗尽自动暂停，充值即恢复。
 *
 * 数据：
 *   GET  /admin/account/sms/haozhuma/watch         状态+配置+日志
 *   PUT  /admin/account/sms/haozhuma/watch         整体保存配置
 *   POST /admin/account/sms/haozhuma/watch/budget  充值/重置额度
 */
export default function WatchCard({ api, h5Ready, onConfigure }) {
  const [watch, setWatch] = useState(null); // GET /watch 结果
  const [watchError, setWatchError] = useState('');
  const [saving, setSaving] = useState(false);
  const [budgetAdding, setBudgetAdding] = useState(false);
  const [addAmount, setAddAmount] = useState(10);
  const [projectPickerOpen, setProjectPickerOpen] = useState(false);
  // 草稿项目列表（编辑中；保存时整体 PUT）。
  const [draftProjects, setDraftProjects] = useState(null);
  const [draftParams, setDraftParams] = useState({});
  const [projectsDirty, setProjectsDirty] = useState(false);
  // 分组选项（与手动加号卡片同一接口；加载失败保持禁用+占位）。
  const [groupOptions, setGroupOptions] = useState(null);
  const [groupsFailed, setGroupsFailed] = useState(false);
  const timerRef = useRef(null);

  const load = async () => {
    try {
      const body = await api('/admin/account/sms/haozhuma/watch');
      setWatch(body);
      setWatchError('');
    } catch (err) {
      setWatchError(err.message);
    }
  };

  useEffect(() => {
    load();
    timerRef.current = setInterval(load, WATCH_POLL_MS);
    return () => clearInterval(timerRef.current);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // 分组选项：进入时拉一次（值班加的号登记进哪个分组）。
  useEffect(() => {
    let alive = true;
    api('/admin/groups')
      .then(body => {
        if (!alive) return;
        setGroupOptions((body.groups || []).filter(g => !g.region || g.region === 'all' || g.region === 'cn').map(g => g.name));
      })
      .catch(() => alive && setGroupsFailed(true));
    return () => { alive = false; };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // 草稿与服务器同步：打开编辑时（watch 变化且草稿未动）刷新。
  useEffect(() => {
    if (watch && !projectsDirty) {
      setDraftProjects(watch.projects || []);
    }
  }, [watch, projectsDirty]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!h5Ready) {
    return <Card title="配置豪猪监控"><Alert showIcon type="info" message="需要豪猪接码配置和 Web 登录令牌" description="在接码平台卡片的豪猪增强功能中保存 Web 登录令牌。" /><Space style={{ marginTop: 16 }}><Button onClick={onConfigure}>去工作台配置豪猪</Button><Button disabled={!watch?.enabled || saving} onClick={async()=>{setSaving(true);try{const r=await api('/admin/account/sms/haozhuma/watch',{method:'PATCH',body:JSON.stringify({enabled:false})});setWatch(r.watch);}catch(e){message.error(e.message);}finally{setSaving(false);}}}>暂停已保存的监控</Button></Space></Card>;
  }

  const available = watch?.available !== false;
  if (!available) {
    return (
      <Card title="监控加号" size="small">
        <Alert
          type="warning"
          showIcon
          message="监控不可用"
          description="需配置豪猪接码账号并粘贴 Web 登录令牌（qs_web_token）后才能监控对接码市场。"
        />
      </Card>
    );
  }

  const projects = draftProjects ?? watch?.projects ?? [];
  const dirty = Object.keys(draftParams).length > 0 || (draftProjects !== null && JSON.stringify(draftProjects) !== JSON.stringify(watch?.projects || []));

  const paused = !!watch?.paused_reason;
  const running = !!watch?.running;
  const enabled = !!watch?.enabled;
  const hasProjects = (watch?.projects || []).length > 0;

  const statusTag = !enabled
    ? <Tag>未启用</Tag>
    : running
      ? <Tag color="processing">加号中</Tag>
      : paused
        ? <Tag color="orange">已暂停</Tag>
        : <Tag color="green">监控中</Tag>;

  const save = async (next) => {
    setSaving(true);
    try {
      const body = await api('/admin/account/sms/haozhuma/watch', {
        method: 'PUT',
        body: JSON.stringify(next),
      });
      if (!body.ok) throw new Error(body.error || '保存失败');
      setWatch(body.watch);
      setDraftProjects(null); setDraftParams({}); setProjectsDirty(false);
      message.success('监控设置已保存并即时生效');
    } catch (err) {
      message.error(`保存失败：${err.message}`);
    } finally {
      setSaving(false);
    }
  };

  const toggleEnabled = async (on) => {
    if (on && (watch?.projects || []).length === 0) {
      message.warning('请先保存至少一个监控项目再开启');
      return;
    }
    setSaving(true);
    try {
      const r = await api('/admin/account/sms/haozhuma/watch', { method:'PATCH', body:JSON.stringify({enabled:on}) });
      setWatch(r.watch); message.success(on ? '监控已开启，使用已保存配置' : '监控已暂停；当前批次请在工作台单独停止');
    } catch(e) { message.error(e.message); }
    finally { setSaving(false); }
  };

  // saveParam 用当前配置 + 覆盖项整体保存（参数区控件共用）。
  const saveParam = patch => setDraftParams(cur => ({...cur,...patch}));
  const shown = {...watch,...draftParams};

  const saveProjects = async () => {
    // 客户端先校验一遍（后端还会再验）：给用户即时反馈。
    for (const p of projects) {
      if (!(p.max_price > 0)) { message.error(`项目 ${p.sid || p.name} 的最高价需大于 0`); return; }
      if (!(p.min_stock >= 1)) { message.error(`项目 ${p.sid || p.name} 的最少库存需 ≥ 1`); return; }
    }
    await save({
      enabled: watch?.enabled || false,
      interval_seconds: watch?.interval_seconds || 0,
      want_per_trigger: watch?.want_per_trigger || 1,
      workers: watch?.workers || 1,
      groups: watch?.groups || [],
      projects,
      ...draftParams,
    });
  };

  const addBudget = async () => {
    if (!(addAmount > 0)) { message.error('预算金额需大于 0'); return; }
    setBudgetAdding(true);
    try {
      const body = await api('/admin/account/sms/haozhuma/watch/budget', {
        method: 'POST',
        body: JSON.stringify({ add: addAmount }),
      });
      if (!body.ok) throw new Error(body.error || '增加预算失败');
      message.success(`已增加预估预算 ¥${addAmount}，额度恢复为 ¥${body.budget_remaining.toFixed(2)}`);
      load();
    } catch (err) {
      message.error(`增加预算失败：${err.message}`);
    } finally {
      setBudgetAdding(false);
    }
  };

  const resetBudget = async () => {
    try {
      const body = await api('/admin/account/sms/haozhuma/watch/budget', {
        method: 'POST',
        body: JSON.stringify({ set: 0 }),
      });
      if (!body.ok) throw new Error(body.error || '重置失败');
      message.success('额度已清零');
      load();
    } catch (err) {
      message.error(`重置失败：${err.message}`);
    }
  };

  const updateProject = (idx, patch) => {
    setProjectsDirty(true);
    setDraftProjects(cur => (cur || watch?.projects || []).map((p, i) => (i === idx ? { ...p, ...patch } : p)));
  };

  const removeProject = (idx) => {
    setProjectsDirty(true);
    setDraftProjects(cur => (cur || watch?.projects || []).filter((_, i) => i !== idx));
  };

  const pickProject = (p) => {
    setProjectsDirty(true);
    if (!p || !p.project_id || !p.sid) {
      message.warning('该项目缺少数字 ID 或会话标识，无法监控');
      return;
    }
    setDraftProjects(cur => [...(cur || watch?.projects || []), {
      sid: p.project_id,
      hex_sid: p.sid,
      name: p.name,
      max_price: 1.0,
      min_stock: 5,
      enabled: true,
    }]);
    message.info('已加入草稿，调好参数后点「保存监控设置」');
  };

  const columns = [
    { title: '项目', dataIndex: 'name', key: 'name', render: (n, p) => <Text>{n || p.sid}</Text> },
    {
      title: '最高价（元）',
      key: 'max_price',
      width: 120,
      render: (_, p, i) => (
        <InputNumber
          min={0.01}
          step={0.1}
          value={p.max_price}
          disabled={saving} onChange={v => updateProject(i, { max_price: v })}
          size="small"
          style={{ width: '100%' }}
        />
      ),
    },
    {
      title: '最少库存',
      key: 'min_stock',
      width: 100,
      render: (_, p, i) => (
        <InputNumber
          min={1}
          value={p.min_stock}
          precision={0} disabled={saving} onChange={v => updateProject(i, { min_stock: v })}
          size="small"
          style={{ width: '100%' }}
        />
      ),
    },
    {
      title: '监控',
      key: 'enabled',
      width: 70,
      render: (_, p, i) => (
        <Switch size="small" checked={p.enabled} disabled={saving} onChange={v => updateProject(i, { enabled: v })} />
      ),
    },
    {
      title: '',
      key: 'op',
      width: 60,
      render: (_, p, i) => (
        <Popconfirm title="移除该监控项目？" onConfirm={() => removeProject(i)}>
          <Button size="small" danger type="text" disabled={saving}>移除</Button>
        </Popconfirm>
      ),
    },
  ];

  const budget = watch?.budget_remaining ?? 0;
  const spent = watch?.spent_total ?? 0;
  const enrolled = watch?.enrolled_total ?? 0;

  return (
    <>
    <Card
      title="监控加号（对接码市场值班员）"
      size="small"
      extra={(
        <Space>
          {statusTag}
          <Switch
            checked={enabled}
            loading={saving}
            checkedChildren="开"
            unCheckedChildren="关"
            onChange={toggleEnabled}
          />
        </Space>
      )}
    >
      {paused && (
        <Alert
          type="warning"
          showIcon
          message={watch.paused_reason}
          style={{ marginBottom: 12 }}
        />
      )}
      {!enabled && hasProjects && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="项目已配好但总开关未开"
          description={(
            <span>
              打开右上角「开」才会开始定期拉取对接码市场并自动加号。项目行里的小开关只控制
              单个项目，不会启动值班。
            </span>
          )}
        />
      )}
      {watchError && (
        <Alert type="error" showIcon message="状态加载失败" description={watchError} style={{ marginBottom: 12 }} />
      )}

      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        {/* 额度 + 统计 */}
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 24, alignItems: 'center' }}>
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block' }}>剩余预估预算</Text>
            <Text strong style={{ fontSize: 22, color: budget <= 0 ? '#cf1322' : undefined }}>
              ¥{budget.toFixed(2)}
            </Text>
          </div>
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block' }}>预估支出 / 已加号</Text>
            <Text style={{ fontSize: 22 }}>¥{spent.toFixed(2)} · {enrolled} 个</Text>
          </div>
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block' }}>增加预算</Text>
            <Space.Compact>
              <InputNumber
                min={1}
                value={addAmount}
                onChange={setAddAmount}
                addonAfter="元"
                style={{ width: 140 }}
              />
              <Button type="primary" loading={budgetAdding} onClick={addBudget}>增加预算</Button>
              <Popconfirm title="清零未来预算？当前已开始的批次不会自动停止。" onConfirm={resetBudget}>
                <Button danger>清零</Button>
              </Popconfirm>
            </Space.Compact>
          </div>
        </div>

        {/* 触发参数 */}
        <Space wrap size={16} align="end">
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>
              每次触发加号数（1-10，编辑后统一保存）
            </Text>
            <CommitInputNumber
              min={1}
              max={10}
              value={shown?.want_per_trigger || 1}
              validate={v => v >= 1 && v <= 10}
              formatMessage={() => '每次触发加号数需在 1-10 之间'}
              onCommit={v => saveParam({ want_per_trigger: v })}
              precision={0} disabled={saving} style={{ width: 120 }}
            />
          </div>
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>
              拉取间隔（秒，60-3600；0=默认 300）
            </Text>
            <CommitInputNumber
              min={0}
              max={3600}
              value={shown?.interval_seconds || 0}
              validate={v => v === 0 || (v >= 60 && v <= 3600)}
              formatMessage={() => '拉取间隔需 60-3600 秒（0 = 默认 300）'}
              onCommit={v => saveParam({ interval_seconds: v })}
              precision={0} disabled={saving} style={{ width: 120 }}
            />
          </div>
          <div>
            <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>
              <Tooltip title="值班加的号登记进这些分组（可多选）。分组密钥只能用对应分组里的账号；空 = default。">
                <span style={{ borderBottom: '1px dashed #bfbfbf' }}>加号进分组</span>
              </Tooltip>
            </Text>
            <Select
              mode="multiple"
              value={shown?.groups?.length ? shown.groups : ['default']}
              disabled={saving || groupsFailed} onChange={v => saveParam({ groups: v.length ? v : ['default'] })}
              style={{ minWidth: 200 }}
              placeholder="default"
              loading={!groupOptions && !groupsFailed}
              status={groupsFailed ? 'warning' : undefined}
              options={(groupOptions || []).map(g => ({ value: g, label: g }))}
              notFoundContent={groupsFailed ? '分组服务不可用，将保存 default' : '暂无分组'}
            />
          </div>
        </Space>

        {/* 项目列表 */}
        <div>
          <Space style={{ marginBottom: 8 }}>
            <Button size="small" icon={<PlusOutlined />} disabled={saving} onClick={() => setProjectPickerOpen(true)}>
              添加监控项目
            </Button>
            <Button
              size="small"
              type="primary"
              disabled={!dirty || saving}
              loading={saving}
              onClick={saveProjects}
            >
              保存监控设置
            </Button>
            {dirty && <Text type="warning">有未保存的修改，监控仍使用原配置</Text>}
          </Space>
          {projects.length === 0 ? (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={(
                <Text type="secondary">
                  还没有监控项目。添加后，值班员会定期拉取这些项目的对接码市场，
                  发现「新出现的低价码 / 降价进入心理价 / 补货」就自动用该码加号。
                </Text>
              )}
            />
          ) : (
            <Table
              size="small"
              rowKey="sid"
              columns={columns}
              dataSource={projects}
              pagination={false}
            />
          )}
        </div>

        {/* 活动日志 */}
        <div>
          <Text type="secondary" style={{ fontSize: 12 }}>
            <EyeOutlined /> 最近活动
            {watch?.last_tick ? `（上次拉取 ${watch.last_tick}）` : ''}
            {watch?.next_tick ? ` · 下次 ${watch.next_tick}` : ''}
          </Text>
          <div style={{
            marginTop: 6, maxHeight: 160, overflowY: 'auto',
            background: '#fafafa', padding: '8px 12px', borderRadius: 6,
            fontFamily: 'ui-monospace, Consolas, monospace', fontSize: 12,
          }}>
            {(watch?.logs || []).length === 0 ? (
              <Text type="secondary">暂无活动</Text>
            ) : (watch?.logs || []).slice().reverse().map((line, i) => (
              <div key={i} style={{
                color: line.includes('失败') || line.includes('暂停') ? '#cf1322'
                  : line.includes('完成') || line.includes('触发') ? '#389e0d' : undefined,
              }}>{line}</div>
            ))}
          </div>
        </div>
      </Space>

      <ProjectPickerModal
        open={projectPickerOpen}
        onClose={() => setProjectPickerOpen(false)}
        api={api}
        onPick={pickProject}
      />
    </Card>

    {/* 成果面板：同一份数据源（10s 轮询），独立卡片更聚焦"看结果" */}
    <WatchHistoryPanel watch={watch} />
    </>
  );
}
