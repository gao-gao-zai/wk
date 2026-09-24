import React, { useEffect, useRef, useState } from 'react';
import {
  Alert, Button, Card, Checkbox, Collapse, Divider, Empty, Input, InputNumber,
  Popconfirm, Progress, Select, Space, Statistic, Tag, Tooltip, Typography, message,
} from 'antd';
import {
  ApiOutlined, CheckCircleOutlined, LinkOutlined, PauseCircleOutlined,
  PlayCircleOutlined, ReloadOutlined, SettingOutlined,
} from '@ant-design/icons';

import AutoEnrollAdvanced from './AutoEnrollAdvanced';

const { Text, Paragraph } = Typography;

// POLL_MS 任务状态轮询间隔（2s：带 200 行日志的接口打得太密没必要）。
const POLL_MS = 2000;
// SUMMARY_MS 账户概览慢轮询（30s）：余额/占用是免费接口但没必要高频打，
// 且与任务状态的 2s 快轮询是两个时钟两个接口，不合并。
const SUMMARY_MS = 30000;
// MAX_WORKERS 与后端 maxWorkers 一致（超出会被后端夹住，这里先拦一层）。
const MAX_WORKERS = 8;
// MAX_POLL_COUNT / MAX_ATTEMPTS 与后端 maxPollCount / maxAttemptsLimit 一致。
// 前端先拦一道，用户拿到的是明确报错而不是被静默夹住。
const MAX_POLL_COUNT = 36;
const MAX_ATTEMPTS = 2000;
// DEFAULT_POLL_COUNT 与后端 defaultPollCount 一致（90s / 5s）。
const DEFAULT_POLL_COUNT = 18;
// 每号轮询次数 → 大约多少秒（间隔 5s）。给用户一个能对照验证码有效期的数字。
const pollSeconds = n => Math.round((Number(n) || 0) * 5);
// 豪猪项目单价（估）：余额换算"约可成功 N 个"用。52283 腾讯项目 2.2 元/次。
const PRICE_PER_SUCCESS = 2.2;

// logTone 给日志行着色：扫一眼就能看出这轮顺不顺。
function logTone(text) {
  if (text.includes('加号成功')) return '#389e0d';
  if (text.includes('失败') || text.includes('终止')) return '#cf1322';
  if (text.includes('取号')) return '#667085';
  return undefined;
}

// 脱敏账号还原展示：豪猪回显 user 形如 "my***er"，直接展示即可。
// authText 把鉴权状态拼成一句话，折叠区顶部/空态共用。
function authSummary(auth) {
  if (!auth || auth.mode === 'none') return '尚未配置豪猪 API 凭据';
  if (auth.mode === 'token') return `Token 模式${auth.has_token ? '（已保存）' : ''}`;
  return `账号密码模式（${auth.user || '?'}）`;
}

/**
 * AutoEnroll 自动加号页。
 *
 * 数据来源：
 *   POST /admin/account/sms/auto-enroll        启动（{count, workers, poll_count, ...}）
 *   GET  /admin/account/sms/auto-enroll        进度（logs / balance / stop_reason）
 *   POST /admin/account/sms/auto-enroll/stop   停止
 *   GET  /admin/account/sms/haozhuma/summary  账户概览（余额/占用/账本）
 *   POST /admin/account/sms/haozhuma/verify   凭据试算（不保存）
 *   POST /admin/config                        豪猪/任务控制参数保存
 *
 * 布局四块（视觉设计文档 §1）：账户卡片 → 启动卡片（含统计条）→ 告警 → 日志。
 * 高级设置是 Drawer（AutoEnrollAdvanced），不在本文件。
 */
export default function AutoEnroll({ api, config, onSaveHaozhuma, onSaveAutoEnroll, onToggleGrowthTasks }) {
  const hz = config.sms?.haozhuma || {};
  const auth = hz.auth || { mode: 'none' };
  const autoenrollCfg = config.autoenroll || {};

  // —— 任务参数（启动卡片） ——
  const [count, setCount] = useState(5);
  const [workers, setWorkers] = useState(3);
  // pollCount 每号收码轮询次数；0 表示交给后端默认（18 次 ≈ 90 秒）。
  const [pollCount, setPollCount] = useState(DEFAULT_POLL_COUNT);
  // maxAttempts 总尝试次数上限；null/0 表示按目标数推导（count*12，下限 20）。
  const [maxAttempts, setMaxAttempts] = useState(null);
  // groups 任务新账号登记的分组（多选，默认 default）。
  const [selectedGroups, setSelectedGroups] = useState(['default']);
  const [groupOptions, setGroupOptions] = useState(null);

  // —— 任务状态 ——
  const [status, setStatus] = useState(null);
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [error, setError] = useState('');
  const [loadingStatus, setLoadingStatus] = useState(false);
  const logBoxRef = useRef(null);
  // 轮询句柄。running 时按 POLL_MS 续期；停下就不再排期。
  const timerRef = useRef(null);

  // —— 账户概览（独立慢时钟） ——
  const [summary, setSummary] = useState(null);
  const [summaryLoading, setSummaryLoading] = useState(false);
  const summaryTimerRef = useRef(null);

  // —— 高级设置抽屉 ——
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const load = async () => {
    setLoadingStatus(true);
    try {
      const body = await api('/admin/account/sms/auto-enroll');
      setStatus(body);
      setError('');
      return body;
    } catch (err) {
      setError(err.message);
      return null;
    } finally {
      setLoadingStatus(false);
    }
  };

  const loadSummary = async () => {
    setSummaryLoading(true);
    try {
      setSummary(await api('/admin/account/sms/haozhuma/summary'));
    } catch {
      // 未配置豪猪/网络抖动：保持上次值，卡片 Tag 会显示"未连接"。
      setSummary(current => current);
    } finally {
      setSummaryLoading(false);
    }
  };

  // 首屏载入 + 自续期轮询：只要后端还在跑就继续轮询，停了自然结束。
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      const body = await load();
      if (!alive) return;
      if (body?.running) {
        timerRef.current = setTimeout(tick, POLL_MS);
      } else {
        timerRef.current = null;
      }
    };
    tick();
    return () => {
      alive = false;
      if (timerRef.current) clearTimeout(timerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 账户概览：进入页面拉一次 + 任务运行期间 30s 慢轮询（跟钱有关的数字
  // 在跑任务时值得盯），空闲时不再自动刷（手动刷新按钮兜底）。
  useEffect(() => {
    loadSummary();
    return () => {
      if (summaryTimerRef.current) clearTimeout(summaryTimerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    if (status?.running) {
      if (!summaryTimerRef.current) {
        const tick = async () => {
          await loadSummary();
          summaryTimerRef.current = setTimeout(tick, SUMMARY_MS);
        };
        summaryTimerRef.current = setTimeout(tick, SUMMARY_MS);
      }
    } else if (summaryTimerRef.current) {
      clearTimeout(summaryTimerRef.current);
      summaryTimerRef.current = null;
    }
  }, [status?.running]);

  // 日志自动滚到底：任务跑起来后最关心的是最新几行。
  useEffect(() => {
    const box = logBoxRef.current;
    if (box) box.scrollTop = box.scrollHeight;
  }, [status?.logs]);

  // 分组选项：加载失败（老后端没有分组端点）时**保持选择器占位并禁用**，
  // 不隐藏——控件闪现/消失比禁用更让人不安。行为退回"新账号进 default"。
  const [groupsFailed, setGroupsFailed] = useState(false);
  useEffect(() => {
    let alive = true;
    api('/admin/groups')
      .then(body => {
        if (!alive) return;
        const names = (body.groups || []).map(g => g.name);
        setGroupOptions(names);
        setSelectedGroups(current => (
          current.every(g => names.includes(g)) ? current : current.filter(g => names.includes(g)).length ? current.filter(g => names.includes(g)) : ['default']
        ));
      })
      .catch(() => alive && setGroupsFailed(true));
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const start = async () => {
    setStarting(true);
    setError('');
    try {
      const body = await api('/admin/account/sms/auto-enroll', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          count,
          workers,
          poll_count: Number(pollCount) || 0,
          max_attempts: Number(maxAttempts) || 0,
          groups: selectedGroups.length ? selectedGroups : ['default'],
        }),
      });
      // 提示用后端回的**生效值**：填了超范围的值时，这里显示的就是真实生效的那个。
      const effPoll = body?.poll_count ?? pollCount;
      message.success(
        `已启动：目标 ${count} 个，并发 ${body?.workers ?? workers}，每号轮询 ${effPoll} 次（约 ${pollSeconds(effPoll)} 秒）`,
      );
      const st = await load();
      // 上一轮轮询可能已结束（timerRef 为 null），重新排期继续跟进度。
      if (body?.running && !timerRef.current) {
        const tick = async () => {
          const next = await load();
          if (next?.running) timerRef.current = setTimeout(tick, POLL_MS);
          else timerRef.current = null;
        };
        timerRef.current = setTimeout(tick, POLL_MS);
      }
      if (!st) return;
    } catch (err) {
      // 余额不足 / 已在运行 都走这里；后端已给出可读文案。
      setError(err.message);
      message.error(err.message);
    } finally {
      setStarting(false);
    }
  };

  const stop = async () => {
    setStopping(true);
    try {
      const body = await api('/admin/account/sms/auto-enroll/stop', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason: '控制台手动停止' }),
      });
      if (body.ok) message.success('已请求停止，正在收尾');
      else message.info(body.note || '当前没有正在运行的任务');
      await load();
    } catch (err) {
      message.error(err.message);
    } finally {
      setStopping(false);
    }
  };

  // releaseAll 一键释放豪猪名下所有占用号码（cancelAllRecv），同时清空
  // 本地账本。额度被历史遗留号占满时的手动兜底——语义同豪猪后台的
  // 「释放全部」按钮。任务运行中后端会拒绝（409）。
  const [releasingAll, setReleasingAll] = useState(false);
  const releaseAll = async () => {
    setReleasingAll(true);
    try {
      const body = await api('/admin/account/sms/release-all', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      if (body.ok) {
        message.success(`已释放豪猪名下全部占用号码（含账本遗留 ${body.ledger_cleared || 0} 个）`);
      }
      await load();
      await loadSummary();
    } catch (err) {
      message.error(err.message);
    } finally {
      setReleasingAll(false);
    }
  };

  const running = status?.running === true;
  const ok = Number(status?.ok || 0);
  const attempts = Number(status?.attempts || 0);
  const fail = Number(status?.fail || 0);
  const consumed = Number(status?.consumed || 0);
  const holding = Number(status?.held || 0);
  const goal = Number(count || 0);
  const percent = goal > 0 ? Math.min(100, Math.round((ok / goal) * 100)) : 0;
  const successRate = attempts > 0 ? Math.round((ok / attempts) * 100) : 0;
  const logs = status?.logs || [];

  // 高级设置"偏离默认值"计数（入口按钮徽标用）：后端已回显当前值，
  // 前端纯本地比较，不打接口。
  const advancedDirtyCount = [
    hz.uid && hz.uid !== '',
    hz.isp && hz.isp !== '',
    hz.author && hz.author !== '',
    Number(autoenrollCfg.min_balance) !== 2.2,
    Number(autoenrollCfg.consecutive_fails) !== 15,
    Number(autoenrollCfg.retry_delay_seconds) !== 5,
  ].filter(Boolean).length;

  // —— 账户卡片显示值：概览接口优先（含实时余额/占用），没拉到时用配置回显。
  const balance = summary?.balance ?? null;
  const occupied = summary?.occupied ?? -1; // -1 = 平台未返回
  const heldLocal = summary?.held_local ?? holding;
  const connected = !!summary && auth.mode !== 'none';
  const approxCount = balance != null && balance > 0
    ? Math.floor(balance / (Number(autoenrollCfg.min_balance) > 0 ? Number(autoenrollCfg.min_balance) : PRICE_PER_SUCCESS))
    : null;

  // 账户 Tag：连接状态一眼可读。未配置/未拉到概览时灰色，凭据在但查询
  // 失败也是灰色（不能因为一次网络抖动就红——豪猪侧波动很常见）。
  const accountTag = auth.mode === 'none'
    ? <Tag>未配置</Tag>
    : connected
      ? <Tag color="green">已连接</Tag>
      : <Tag color="orange">连接异常</Tag>;

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Alert
        type="info"
        showIcon
        message="按成功计费，失败不扣费"
        description="豪猪只在收到短信验证码时扣费；取号后收不到码不扣费。每个取到的号（成功或失败）都会加入黑名单，不会被重复取到。"
      />

      {error && <Alert type="error" showIcon message="操作失败" description={error} />}

      {/* —— 卡片① 豪猪账户 —— */}
      <Card
        title="豪猪账户"
        extra={<Space>{accountTag}<Button size="small" icon={<ReloadOutlined />} loading={summaryLoading} onClick={loadSummary} /></Space>}
      >
        {auth.mode === 'none' && !connected ? (
          <Paragraph type="secondary" style={{ marginBottom: 8 }}>
            自动加号需要豪猪接码平台账户。展开下方「鉴权与增强设置」填入 API 账号密码即可开始使用。
            还没有账户？<a href="https://www.haozhuma.com" target="_blank" rel="noreferrer">去豪猪注册 <LinkOutlined /></a>
          </Paragraph>
        ) : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(160px,1fr))', gap: 16 }}>
            <Statistic
              title="余额"
              value={balance != null ? balance.toFixed(2) : '—'}
              suffix={balance != null ? '元' : ''}
              valueStyle={{ color: Number(autoenrollCfg.min_balance) > 0 && balance != null && balance < Number(autoenrollCfg.min_balance) ? '#cf1322' : undefined, fontSize: 22 }}
            />
            <div>
              <Statistic
                title="号码占用（平台侧）"
                value={occupied >= 0 ? occupied : '未知'}
                valueStyle={{ fontSize: 22, color: occupied >= 0 && occupied !== heldLocal ? '#d46b08' : undefined }}
              />
              <Text type="secondary" style={{ fontSize: 12 }}>
                本地账本 {heldLocal} 个{occupied >= 0 && occupied !== heldLocal ? '（不一致：有未归还号，可下方一键释放）' : ''}
              </Text>
            </div>
            <div>
              <Statistic title="项目" value={hz.sid || '未配置'} valueStyle={{ fontSize: 22 }} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                {approxCount != null ? `约可成功 ${approxCount} 个` : ''}
              </Text>
            </div>
          </div>
        )}
        <Collapse
          ghost
          defaultActiveKey={auth.mode === 'none' ? ['auth'] : []}
          style={{ marginTop: 4, marginLeft: -16, marginRight: -16 }}
          items={[{
            key: 'auth',
            label: <Text type="secondary">鉴权与增强设置（{authSummary(auth)}）</Text>,
            children: (
              <AuthPanel
                api={api}
                auth={auth}
                running={running}
                onSaveHaozhuma={onSaveHaozhuma}
                onSaved={() => { loadSummary(); }}
              />
            ),
          }]}
        />
      </Card>

      {/* —— 卡片② 启动任务（参数 + 主按钮 + 统计条 + 进度） —— */}
      <Card
        title="启动任务"
        extra={<Tag color={running ? 'processing' : 'default'}>{running ? '运行中' : '空闲'}</Tag>}
      >
        <Space wrap size={16} align="end">
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>目标账号数</Text>
            <InputNumber min={1} max={50} value={count} onChange={setCount} disabled={running} style={{ width: 130 }} />
          </div>
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>并发数</Text>
            <InputNumber min={1} max={MAX_WORKERS} value={workers} onChange={setWorkers} disabled={running} style={{ width: 130 }} />
          </div>
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>
              <Tooltip title="每个号最多查几次短信。腾讯验证码 60 秒有效，但豪猪短信入库有延迟，默认 18 次（约 90 秒）能兜住晚到的码。收不到码时可调大。">
                <span style={{ borderBottom: '1px dashed #bfbfbf' }}>收码轮询次数</span>
              </Tooltip>
            </Text>
            <InputNumber
              min={1}
              max={MAX_POLL_COUNT}
              value={pollCount}
              onChange={setPollCount}
              disabled={running}
              style={{ width: 160 }}
              addonAfter={`≈${pollSeconds(pollCount)}秒`}
            />
          </div>
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>
              <Tooltip title="整个任务最多尝试多少个号。留空按目标数推导（目标×12，最少 20）。对接商质量差、成功率低时可以调大，但连续失败熔断仍会兜底。">
                <span style={{ borderBottom: '1px dashed #bfbfbf' }}>最多尝试次数</span>
              </Tooltip>
            </Text>
            <InputNumber
              min={1}
              max={MAX_ATTEMPTS}
              value={maxAttempts}
              onChange={setMaxAttempts}
              disabled={running}
              placeholder={`自动（${Math.max(20, count * 12)}）`}
              style={{ width: 160 }}
            />
          </div>
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>
              <Tooltip title="新加的账号登记进这些分组（可多选）。分组密钥只能用对应分组里的账号。">
                <span style={{ borderBottom: '1px dashed #bfbfbf' }}>所属分组</span>
              </Tooltip>
            </Text>
            <Select
              mode="multiple"
              value={selectedGroups}
              onChange={setSelectedGroups}
              disabled={running}
              style={{ minWidth: 220 }}
              placeholder="default"
              loading={!groupOptions && !groupsFailed}
              status={groupsFailed ? 'warning' : undefined}
              options={(groupOptions || []).map(g => ({ value: g, label: g }))}
              notFoundContent={groupsFailed ? '分组服务不可用，新账号将进 default' : '暂无分组'}
            />
          </div>
          <Button type="primary" icon={<PlayCircleOutlined />} loading={starting} disabled={running} onClick={start}>
            开始加号
          </Button>
          <Tooltip title={running ? '请求停止当前任务' : '当前没有运行中的任务'}>
            <Button icon={<PauseCircleOutlined />} danger loading={stopping} disabled={!running} onClick={stop}>
              停止
            </Button>
          </Tooltip>
          <Button icon={<ReloadOutlined />} loading={loadingStatus} onClick={load}>刷新</Button>
          <Tooltip title="调用豪猪「释放全部」（cancelAllRecv），释放账户名下所有占用中的号码，并清空本地遗留账本。额度被旧号占满时的手动兜底；任务运行中不可用。">
            <Button loading={releasingAll} disabled={running} onClick={releaseAll}>
              一键释放全部号码
            </Button>
          </Tooltip>
          <Button type="link" icon={<SettingOutlined />} onClick={() => setAdvancedOpen(true)}>
            高级设置{advancedDirtyCount > 0 ? `（${advancedDirtyCount}）` : ''}
          </Button>
        </Space>

        <Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
          并发越高越快，但腾讯对同批次注册有风控，建议 3-4。每个号开始时会绑定一个专属代理出口，全程不换 IP。
          {' '}收不到码时先调大「收码轮询次数」——豪猪短信入库有延迟，多轮几次常能捞到。
        </Paragraph>

        <Divider style={{ margin: '12px 0' }} />
        <Checkbox
          checked={config.autoenroll_growth_tasks !== false}
          onChange={e => onToggleGrowthTasks?.(e.target.checked)}
        >
          加号成功后自动跑成长任务
          <Text type="secondary" style={{ marginLeft: 8, fontWeight: 400 }}>
            （约 +1950 积分/号；异步执行不占加号并发；含数条真实短对话）
          </Text>
        </Checkbox>

        {(attempts > 0 || running) && (
          <>
            <Divider style={{ margin: '12px 0 8px' }} />
            <Space split={<Divider type="vertical" />} wrap size={16}>
              <Statistic title="本次成功" value={ok} valueStyle={{ fontSize: 18, color: '#389e0d' }} />
              <Statistic title="尝试" value={attempts} valueStyle={{ fontSize: 18 }} />
              <Statistic title="成功率" value={successRate} suffix="%" valueStyle={{ fontSize: 18 }} />
              <Statistic title="号码消耗" value={consumed} valueStyle={{ fontSize: 18 }} />
              <Statistic title="已释放" value={status?.released ?? 0} valueStyle={{ fontSize: 18 }} />
              <Statistic
                title="连续失败熔断"
                value={autoenrollCfg.consecutive_fails ?? 15}
                suffix="次"
                valueStyle={{ fontSize: 18 }}
              />
            </Space>
            {goal > 0 && (
              <Progress
                percent={percent}
                status={running ? 'active' : (ok >= goal ? 'success' : 'normal')}
                format={() => `${ok} / ${goal}`}
                style={{ marginTop: 4 }}
              />
            )}
            <Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}>
              生效参数：并发 {status?.workers ?? workers} · 每号轮询 {status?.poll_count ?? pollCount} 次
              （约 {pollSeconds(status?.poll_count ?? pollCount)} 秒）· 最多尝试 {status?.max_attempts ?? '自动'}
              {status?.max_attempts ? ' 次' : ''} · 余额保护 {Number(autoenrollCfg.min_balance ?? 2.2).toFixed(1)} 元
            </Paragraph>
          </>
        )}
      </Card>

      {holding > 0 && (
        <Alert
          type="error"
          showIcon
          message={`还有 ${holding} 个号码占着豪猪额度`}
          description={(
            <div>
              未归还的号会占住豪猪的并发额度，额度满了后续取号会一直返回「您的余额不足,请释放拉黑后再取号」（看着像没钱，其实是号没还）。
              正常任务结束会自动归还。也可以直接点上方「一键释放全部号码」（豪猪 cancelAllRecv，立即归还全部占用并清空本地账本）。
              <div style={{ marginTop: 8 }}>
                <Button size="small" danger loading={releasingAll} disabled={running} onClick={releaseAll}>
                  一键释放全部号码
                </Button>
              </div>
            </div>
          )}
        />
      )}

      {status?.stop_reason && (
        <Alert type="warning" showIcon message="任务终止原因" description={status.stop_reason} />
      )}

      {/* —— 卡片④ 运行日志 —— */}
      <Card
        title="运行日志"
        extra={(
          <Space>
            <Text type="secondary">{logs.length} 行</Text>
            {logs.length > 0 && (
              <Button size="small" type="text" onClick={() => setStatus(current => ({ ...current, logs: [] }))}>
                清空
              </Button>
            )}
          </Space>
        )}
      >
        {logs.length === 0
          ? <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无日志，启动任务后会实时刷新" />
          : (
            <div
              ref={logBoxRef}
              style={{
                maxHeight: 380, overflowY: 'auto', background: '#fafbfc',
                border: '1px solid #e7ebf1', borderRadius: 8, padding: '10px 12px',
                fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
                fontSize: 12.5, lineHeight: 1.75,
              }}
            >
              {logs.map((line, index) => (
                <div key={index} style={{ color: logTone(line), whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {line}
                </div>
              ))}
            </div>
          )}
      </Card>

      <AutoEnrollAdvanced
        open={advancedOpen}
        onClose={() => setAdvancedOpen(false)}
        api={api}
        config={config}
        running={running}
        onSaveHaozhuma={onSaveHaozhuma}
        onSaveAutoEnroll={onSaveAutoEnroll}
      />
    </Space>
  );
}

/**
 * AuthPanel 鉴权与增强设置折叠区（账户卡片内）。
 *
 * 受控组件风格（与主页一致，不用 antd Form）：
 *   - 鉴权方式二选一：账密（可自动重登）/ 直接填 token
 *   - [验证连接]：POST verify 试算，不保存；结果内联显示（绿字/红字原文）
 *   - [保存并重连]：inline confirm（按钮原地变确认），onSaveHaozhuma 落盘
 *     并触发后端 ReplaceClient；失败 Alert 顶部常驻（回滚由后端保证）
 *   - 密码/token 框：已保存时留空 = 不修改（placeholder 说明）
 */
function AuthPanel({ api, auth, running, onSaveHaozhuma, onSaved }) {
  const [mode, setMode] = useState(auth.mode === 'token' ? 'token' : 'userpass');
  const [userDraft, setUserDraft] = useState('');
  const [passDraft, setPassDraft] = useState('');
  const [tokenDraft, setTokenDraft] = useState('');
  const [verifying, setVerifying] = useState(false);
  const [verifyResult, setVerifyResult] = useState(null); // {ok,balance} | {error}
  const [confirming, setConfirming] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');

  const verify = async () => {
    setVerifying(true);
    setVerifyResult(null);
    try {
      const body = mode === 'userpass'
        ? { mode, user: userDraft || auth.user, pass: passDraft }
        : { mode, token: tokenDraft };
      const resp = await api('/admin/account/sms/haozhuma/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      setVerifyResult({ ok: true, balance: resp.balance });
    } catch (err) {
      setVerifyResult({ error: err.message });
    } finally {
      setVerifying(false);
    }
  };

  const save = async () => {
    setSaving(true);
    setSaveError('');
    const patch = {};
    if (mode === 'userpass') {
      if (userDraft) patch.user = userDraft;
      if (passDraft) patch.pass = passDraft;
    } else if (tokenDraft) {
      patch.token = tokenDraft;
    }
    const result = await onSaveHaozhuma?.(patch);
    setSaving(false);
    setConfirming(false);
    if (result) {
      const updated = result?.updated || {};
      if (updated.haozhuma_auth === 'reconnected') {
        message.success('鉴权已保存，新凭据立即生效');
      } else if (updated.haozhuma_auth_restart_required) {
        message.warning('凭据已保存，重启后自动加号可用（服务启动时未配置豪猪）');
      } else {
        message.success('已保存');
      }
      setUserDraft(''); setPassDraft(''); setTokenDraft(''); setVerifyResult(null);
      onSaved?.();
    }
  };

  const handleSaveClick = () => {
    // 没有任何草稿时保存无意义（后端 patch 全空）。
    const hasDraft = mode === 'userpass' ? !!(userDraft || passDraft) : !!tokenDraft;
    if (!hasDraft) { message.info('没有要保存的改动'); return; }
    if (confirming) { save(); return; }
    setConfirming(true);
  };

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {saveError && <Alert type="error" showIcon closable message="保存失败" description={saveError} onClose={() => setSaveError('')} />}
      <Space wrap size={16} align="end">
        <div>
          <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>鉴权方式</Text>
          <Select
            value={mode}
            onChange={v => { setMode(v); setVerifyResult(null); setConfirming(false); }}
            disabled={running}
            style={{ width: 180 }}
            options={[
              { value: 'userpass', label: '账号密码（可自动重登）' },
              { value: 'token', label: '直接填 Token' },
            ]}
          />
        </div>
        {mode === 'userpass' ? (
          <>
            <div>
              <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>账号</Text>
              <Input
                value={userDraft}
                onChange={e => setUserDraft(e.target.value)}
                placeholder={auth.mode === 'userpass' ? `${auth.user}（已保存，留空不修改）` : '豪猪 API 账号'}
                disabled={running}
                style={{ width: 200 }}
                allowClear
              />
            </div>
            <div>
              <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>密码</Text>
              <Input.Password
                value={passDraft}
                onChange={e => setPassDraft(e.target.value)}
                placeholder={auth.has_pass ? '已保存，留空则不修改' : '豪猪 API 密码'}
                disabled={running}
                style={{ width: 200 }}
              />
            </div>
          </>
        ) : (
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>Token</Text>
            <Input.Password
              value={tokenDraft}
              onChange={e => setTokenDraft(e.target.value)}
              placeholder={auth.has_token ? '已保存，留空则不修改；填写则覆盖' : '豪猪后台提取的 API token'}
              disabled={running}
              style={{ width: 280 }}
            />
          </div>
        )}
        <Tooltip title="用当前填写的草稿真调一次豪猪接口，不保存。建议先验证再保存，避免把坏凭据写进配置。">
          <Button icon={<ApiOutlined />} loading={verifying} onClick={verify}>验证连接</Button>
        </Tooltip>
        {confirming ? (
          <Popconfirm
            title="确认保存？新凭据将立即生效"
            onConfirm={save}
            onCancel={() => setConfirming(false)}
            okText="确认保存"
            cancelText="取消"
            open={confirming}
            disabled={running}
          >
            <Button type="primary" danger loading={saving} disabled={running}>确认保存并重连</Button>
          </Popconfirm>
        ) : (
          <Tooltip title={running ? '任务运行中不可更换鉴权（等任务结束后再试）' : ''}>
            <Button type="primary" loading={saving} disabled={running} onClick={handleSaveClick}>
              保存并重连
            </Button>
          </Tooltip>
        )}
      </Space>
      {verifyResult && (
        verifyResult.ok
          ? <Text type="success">连接成功，当前余额 {Number(verifyResult.balance).toFixed(2)} 元</Text>
          : <Text type="danger" style={{ wordBreak: 'break-all' }}>{verifyResult.error}</Text>
      )}
      <Text type="secondary" style={{ fontSize: 12 }}>
        账密模式 token 失效可自动重登；只填 token 则失效后需手动更新。
        豪猪 API 账密在豪猪网页后台左侧「API」处获取。
      </Text>
    </Space>
  );
}
