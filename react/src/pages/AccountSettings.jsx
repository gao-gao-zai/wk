import React, { useEffect, useRef, useState } from 'react';
import {
  Alert, App, Button, Card, Input, Select, Space, Tag, Typography,
} from 'antd';
import {
  CheckCircleOutlined, KeyOutlined, SafetyOutlined, SendOutlined, ToolOutlined, UnlockOutlined,
} from '@ant-design/icons';
import { orderCaptchaOptions, runTencentCaptcha } from '../captcha';

const { Text, Paragraph } = Typography;

/**
 * AccountSettings 账号配置页（hash 路由 #/accounts）。
 *
 * 从仪表盘"管理设置"里拆出来的账号生命周期操作：
 *  - OAuth 授权链接（中国区 / 海外版）
 *  - 短信直登（中国区，含人机校验回灌）
 *
 * 签到计划、立即签到这类"调度配置"留在管理设置页——它们是运维参数，
 * 与账号增删是两回事，混在一起会让页面越来越长。
 *
 * 数据流：所有状态留在本页（受控组件），api() 与 refresh() 由父级 Console
 * 传入，账号入库后调用 refresh() 让账号池/仪表盘立刻反映新账号。
 */
export default function AccountSettings({ api, refresh, region }) {
  // antd v5 + React 19：静态 message/modal 不可用，必须从 App 上下文拿实例。
  const { message, modal } = App.useApp();
  const [loginRegion, setLoginRegion] = useState('cn');
  const [loginURL, setLoginURL] = useState('');
  const [loginPendingRegion, setLoginPendingRegion] = useState('');
  const [loginPolling, setLoginPolling] = useState(false);
  // 短信直登状态：手机号 → 发码 → 验证码 → 落盘。
  const [smsMobile, setSmsMobile] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [smsSession, setSmsSession] = useState('');
  // smsCaptcha 非空表示这次发码被人机校验拦下：必须先过码才会真的发短信。
  // 形状为 { options: [{appId, cloudType}], reason, auto_attempted }。
  const [smsCaptcha, setSmsCaptcha] = useState(null);
  const [smsCaptchaBusy, setSmsCaptchaBusy] = useState(false);
  const [smsSending, setSmsSending] = useState(false);
  const [smsVerifying, setSmsVerifying] = useState(false);
  const [smsCountdown, setSmsCountdown] = useState(0);
  const loginRegionTouched = useRef(false);
  const loginPollAbort = useRef(false);

  // 后端配置为海外版时默认选中海外区；用户手动选过后不再覆盖。
  useEffect(() => {
    if (!loginRegionTouched.current && region === 'global') setLoginRegion('global');
  }, [region]);

  // 短信验证码重发倒计时：上游 60 秒内会拒绝重复发码。
  useEffect(() => {
    if (smsCountdown <= 0) return undefined;
    const timer = setTimeout(() => setSmsCountdown(value => value - 1), 1000);
    return () => clearTimeout(timer);
  }, [smsCountdown]);

  // 组件卸载时停掉登录轮询，避免后台空转与 setState 泄漏。
  useEffect(() => () => { loginPollAbort.current = true; }, []);

  // startLoginPoll 打开授权链接后自动轮询授权结果。
  // 手动轮询容易忘，而 OAuth 完成后不轮询账号不会入库；这里每 3 秒查一次，
  // 最长约 3 分钟后放弃（避免永久占用一个定时器）。
  const startLoginPoll = async (targetRegion) => {
    loginPollAbort.current = false;
    setLoginPolling(true);
    const query = `?region=${encodeURIComponent(targetRegion)}`;
    let added = false;
    try {
      for (let attempt = 0; attempt < 60; attempt += 1) {
        if (loginPollAbort.current) break;
        // 首次立刻尝试，之后每 3 秒一次。
        if (attempt > 0) await new Promise(resolve => setTimeout(resolve, 3000));
        if (loginPollAbort.current) break;
        try {
          const result = await api(`/admin/account/poll${query}`, { method: 'POST' });
          if (result.ok) {
            if (result.warning) message.warning(result.warning);
            else message.success(`${result.region === 'global' ? '海外版' : '中国区'}账号已添加`);
            added = true;
            break;
          }
        } catch {
          // 单次失败（含 HTTP 409「尚未授权」）继续轮询，不打断。
        }
      }
      if (!added && !loginPollAbort.current) {
        message.warning('自动轮询已超时，完成浏览器授权后可手动点击「轮询授权结果」。');
      }
    } finally {
      setLoginPolling(false);
      setLoginURL('');
      setLoginPendingRegion('');
      await refresh();
    }
  };

  // runSMSSend 发送短信验证码：成功后会拿到 session_id，验码时回传。
  // 同一个按钮同时承担"重发"：上游对 unexpired 的号码不会重复发短信，
  // 只会换发新的 state_token，所以重发是安全的。
  const runSMSSend = async (options = {}) => {
    const mobile = smsMobile.trim();
    if (!mobile) { message.warning('请先填写手机号'); return; }
    const isResend = options.resend === true && !!smsSession;
    setSmsSending(true);
    try {
      const result = await api('/admin/account/sms/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mobile, region: 'cn', force: options.force === true }),
      });
      setSmsSession(result.session_id || '');
      setSmsCode('');
      // 被要求人机校验：还没有真正发码，先把验证码交给用户过。
      if (result.captcha) {
        setSmsCaptcha(result.captcha);
        setSmsCountdown(0);
        message.warning(result.captcha.reason || '该号码需要人机校验，请完成验证');
        return;
      }
      setSmsCaptcha(null);
      // 上游约 60 秒内不接受重复发码；用 expires_in 驱动倒计时更贴近真实窗口。
      const wait = Number(result.expires_in) > 0 ? Math.min(Number(result.expires_in), 60) : 60;
      setSmsCountdown(wait);
      if (result.status === 'unexpired') {
        // 未过期说明短信已经发过了，不会再来一条新的。
        message.success(isResend
          ? '已刷新会话，请使用已收到的那条验证码'
          : `该号码已有未过期验证码，请直接输入${result.expires_in ? `（约 ${Math.ceil(result.expires_in / 60)} 分钟内有效）` : ''}`);
      } else {
        message.success(isResend ? '验证码已重新发送，请查收短信' : '验证码已发送，请查收短信');
      }
    } catch (error) {
      if (error.status === 409 && error.existing) {
        // 该号码已在号池中。后端在发短信之前就拦下了，所以这里还能让用户反悔，
        // 不至于白消耗一条短信。
        const label = error.existingNickname || error.existingUid?.slice(0, 12) || '该账号';
        modal.confirm({
          title: '该手机号已在号池中',
          content: error.existingDisabled
            ? `账号 ${label} 已存在，且当前处于禁用状态。重新登录会更新凭证，但不会自动启用——需要你在账号列表里手动启用后才会接流量。仍要重新登录吗？`
            : `账号 ${label} 已存在。继续登录会刷新它的凭证（积分与冷却状态保持不变），不会新增账号。仍要继续吗？`,
          okText: '继续登录',
          cancelText: '取消',
          onOk: () => runSMSSend({ resend: isResend, force: true }),
        });
      } else if (error.reason === 'too_frequent') {
        // 频控：旧验证码仍然有效，不必清空会话。
        message.warning(`${error.message}（已发出的验证码仍然有效）`);
        setSmsCountdown(30);
      } else {
        message.error(error.message);
      }
    } finally {
      setSmsSending(false);
    }
  };

  // submitCaptcha 把用户完成的验证码票据回传后端，由后端继续发码。
  //
  // 上游会给出多个方案（实测 teg + tencent）。从非 codebuddy.cn 域名发起时，
  // teg 那条会被腾讯以 403 拒绝（SDK 回调带 trerror_* 错误票据），而 tencent
  // 正常。因此这里在某个方案启动失败时自动顺延到下一个，用户不必自己判断
  // 该点哪个——按钮仍然都列出来，供需要时手动选择。
  const submitCaptcha = async (option, options = {}) => {
    if (!smsSession) { message.warning('会话已失效，请重新发送验证码'); return; }
    setSmsCaptchaBusy(true);
    try {
      const { ticket, randStr, cloudType } = await runTencentCaptcha(option.appId, option.cloudType);
      const result = await api('/admin/account/sms/captcha', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: smsSession, ticket, rand_str: randStr, cloud_type: cloudType,
        }),
      });
      // 票据过期时后端会再给一次挑战，此时保留会话让用户重试即可。
      if (result.captcha) {
        setSmsCaptcha(result.captcha);
        message.warning(result.captcha.reason || '验证码已过期，请重新完成校验');
        return;
      }
      setSmsCaptcha(null);
      setSmsCode('');
      const wait = Number(result.expires_in) > 0 ? Math.min(Number(result.expires_in), 60) : 60;
      setSmsCountdown(wait);
      message.success(result.status === 'unexpired'
        ? '校验通过，短信已发送，请查收'
        : '校验通过，验证码已发送，请查收');
    } catch (error) {
      // 用户主动关掉验证码弹窗不该报错，只提示一下即可。
      if (error.cancelled) {
        message.info(error.message);
        return;
      }
      // 该方案启动失败（如 teg 在我们域名下被拒）时自动试下一个方案。
      const rest = (smsCaptcha?.options || []).filter(o => o.appId !== option.appId);
      if (!options.lastResort && rest.length) {
        message.info(`${error.message}，正在尝试其他验证方式…`);
        await submitCaptcha(rest[0], { lastResort: rest.length === 1 });
        return;
      }
      message.error(error.message);
    } finally {
      setSmsCaptchaBusy(false);
    }
  };

  // runSMSVerify 提交验证码：后端会走完 OneID → Keycloak → Console 全部步骤并落盘。
  const runSMSVerify = async () => {
    if (!smsSession) { message.warning('请先发送验证码'); return; }
    const code = smsCode.trim();
    if (!code) { message.warning('请填写收到的验证码'); return; }
    setSmsVerifying(true);
    try {
      const result = await api('/admin/account/sms/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: smsSession, code }),
      });
      // notice 用于"该账号已存在"这类需要用户知晓但不阻断流程的提示；
      // warning 是配置未能持久化等真问题，优先级更高。
      if (result.warning) message.warning(result.warning);
      else if (result.notice) message.info(result.notice);
      else if (result.existed) message.success(`账号 ${result.nickname || result.uid?.slice(0, 12)} 的凭证已更新`);
      else message.success(`账号 ${result.nickname || result.uid?.slice(0, 12)} 已添加`);
      setSmsSession('');
      setSmsCode('');
      setSmsMobile('');
      setSmsCaptcha(null);
      setSmsCountdown(0);
      await refresh();
    } catch (error) {
      message.error(error.message);
      // 还没过码就点了验码：把用户导回验证码步骤，会话仍然可用。
      if (error.reason === 'captcha_pending') {
        setSmsCaptcha(prev => prev || { options: [], reason: '请先完成人机校验' });
        return;
      }
      // 验证码填错/频控时上游的 state_token 依然有效，保留会话让用户直接重填，
      // 不必浪费一条短信。只有会话真的失效（410）才清空并要求重新发码。
      const keepSession = error.retryable === true && error.status !== 410;
      if (keepSession) {
        setSmsCode('');
        if (error.reason === 'too_frequent') setSmsCountdown(5);
      } else {
        setSmsSession('');
        setSmsCaptcha(null);
        setSmsCountdown(0);
      }
    } finally {
      setSmsVerifying(false);
    }
  };

  const poolRegionLabel = region === 'all' ? '混合区域' : region === 'global' ? '海外版' : '中国区';
  const pollRegion = loginPendingRegion || loginRegion;

  return (
    <Card title="账号授权" extra={<Tag color={region === 'all' ? 'green' : 'blue'}>账号池：{poolRegionLabel}</Tag>}>
      <Space wrap>
        <Text type="secondary">登录区域</Text>
        <Select
          value={loginRegion}
          onChange={value => { loginRegionTouched.current = true; setLoginRegion(value); }}
          style={{ width: 150 }}
          options={[{ value: 'cn', label: '中国区' }, { value: 'global', label: '海外版' }]}
        />
        <Button icon={<UnlockOutlined />} onClick={async () => {
          try {
            const query = `?region=${encodeURIComponent(loginRegion)}`;
            const result = await api(`/admin/account/url${query}`, { method: 'POST' });
            setLoginURL(result.url || '');
            setLoginPendingRegion(result.region || loginRegion);
            const popup = window.open(result.url, '_blank', 'noopener');
            message.info(popup
              ? `${loginRegion === 'global' ? '海外版' : '中国区'}授权链接已打开，正在自动等待授权完成…`
              : '浏览器拦截了弹窗，请点击下方授权链接完成登录，系统会自动轮询');
            // 打开链接后自动轮询，避免用户忘记点"轮询授权结果"导致账号没入库。
            startLoginPoll(result.region || loginRegion);
          } catch (error) { message.error(error.message); }
        }}>生成 OAuth 登录链接</Button>
        <Button icon={<ToolOutlined />} loading={loginPolling} onClick={async () => {
          try {
            const query = `?region=${encodeURIComponent(pollRegion)}`;
            const result = await api(`/admin/account/poll${query}`, { method: 'POST' });
            if (result.warning) message.warning(result.warning);
            else message.success(`${result.region === 'global' ? '海外版' : '中国区'}账号已添加`);
            setLoginURL('');
            setLoginPendingRegion('');
            await refresh();
          } catch (error) { message.error(error.message); }
        }}>轮询授权结果</Button>
      </Space>
      {loginURL && (
        <div style={{ marginTop: 12, wordBreak: 'break-all' }}>
          <Text type="secondary">{pollRegion === 'global' ? '海外版' : '中国区'}授权链接：</Text>{' '}
          <a href={loginURL} target="_blank" rel="noreferrer">点击打开浏览器登录</a>
        </div>
      )}
      <Paragraph type="secondary" style={{ margin: '12px 0 0' }}>
        中国区与海外版账号可以同时使用；登录后系统会按账号区域自动选择对应的模型、聊天和积分接口。
        批量加号请使用「自动加号」页。
      </Paragraph>

      <Card
        type="inner"
        title="短信直登（中国区）"
        style={{ marginTop: 16 }}
        extra={<Tag color="orange">免开浏览器</Tag>}
      >
        <Paragraph type="secondary" style={{ marginTop: 0 }}>
          填写手机号后点「发送验证码」，收到短信填入验证码即可直接添加账号，
          无需打开浏览器点授权。仅支持中国区；海外版请用上面的 OAuth 链接。
          若号码被人机校验拦下，已配置打码平台密钥时会自动过码，否则请改用浏览器授权。
        </Paragraph>
        <Space wrap>
          <Input
            value={smsMobile}
            onChange={event => setSmsMobile(event.target.value)}
            placeholder="手机号，如 +8613800138000 或 +852 64087495"
            style={{ width: 300 }}
            prefix={<KeyOutlined />}
            allowClear
            disabled={!!smsSession}
          />
          <Button
            icon={<SendOutlined />}
            loading={smsSending}
            disabled={smsCountdown > 0}
            onClick={() => runSMSSend({ resend: !!smsSession })}
          >
            {smsCountdown > 0
              ? `${smsCountdown} 秒后可重发`
              : (smsSession ? '重新发送验证码' : '发送验证码')}
          </Button>
          {smsSession && (
            <Button
              type="link"
              onClick={() => { setSmsSession(''); setSmsCode(''); setSmsCaptcha(null); setSmsCountdown(0); }}
            >
              换手机号
            </Button>
          )}
        </Space>
        {smsCaptcha && (
          <Alert
            style={{ marginTop: 12 }}
            type="warning"
            showIcon
            message="该号码需要人机校验"
            description={(
              <Space direction="vertical" size={8}>
                <Text type="secondary">
                  {smsCaptcha.reason || '请完成验证码后继续，通过后会自动发送短信。'}
                </Text>
                {/* 上游会给出多种校验方案（teg / tencent）。已知 tencent 可用，
                    因此把它排在前面，避免用户先点到一个注定失败的按钮；
                    teg 仍保留作为后备。 */}
                <Space wrap>
                  {orderCaptchaOptions(smsCaptcha.options).map(option => (
                    <Button
                      key={`${option.cloudType}-${option.appId}`}
                      type="primary"
                      size="small"
                      icon={<SafetyOutlined />}
                      loading={smsCaptchaBusy}
                      onClick={() => submitCaptcha(option)}
                    >
                      {option.cloudType === 'tencent' ? '开始验证（腾讯）' : `开始验证（${option.cloudType}）`}
                    </Button>
                  ))}
                  {!(smsCaptcha.options || []).length && (
                    <Button
                      type="primary" size="small" icon={<SafetyOutlined />}
                      loading={smsCaptchaBusy}
                      onClick={() => submitCaptcha({ appId: '', cloudType: '' })}
                    >
                      重新获取验证码
                    </Button>
                  )}
                </Space>
              </Space>
            )}
          />
        )}
        <Space wrap style={{ marginTop: 12 }}>
          <Input
            value={smsCode}
            onChange={event => setSmsCode(event.target.value)}
            onPressEnter={runSMSVerify}
            placeholder="6 位短信验证码"
            style={{ width: 200 }}
            disabled={!smsSession || !!smsCaptcha}
            allowClear
          />
          <Button
            type="primary"
            icon={<CheckCircleOutlined />}
            loading={smsVerifying}
            disabled={!smsSession || !!smsCaptcha}
            onClick={runSMSVerify}
          >
            验证并添加账号
          </Button>
          {smsCaptcha
            ? <Text type="secondary">请先完成上方的人机校验</Text>
            : smsSession
              ? <Text type="secondary">验证码已发送，请查收；会话 10 分钟内有效</Text>
              : <Text type="secondary">填错验证码不会作废会话，可直接重填</Text>}
        </Space>
      </Card>
    </Card>
  );
}
